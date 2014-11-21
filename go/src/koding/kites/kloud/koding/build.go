package koding

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go.crypto/ssh"

	"koding/kites/kloud/kloud"
	"koding/kites/kloud/machinestate"
	"koding/kites/kloud/protocol"
	"koding/kites/kloud/provider/amazon"

	"github.com/dgrijalva/jwt-go"
	kiteprotocol "github.com/koding/kite/protocol"
	"github.com/koding/logging"
	"github.com/mitchellh/goamz/ec2"
	"github.com/nu7hatch/gouuid"
	"gopkg.in/yaml.v2"
)

const (
	DefaultKloudKeyName = "Kloud"
	DefaultApachePort   = 80
	DefaultKitePort     = 3000
)

var (
	DefaultCustomAMITag = "koding-stable" // Only use AMI's that have this tag

	// Starting from cheapest, list is according to us-east and coming from:
	// http://www.ec2instances.info/. t2.micro is not included because it's
	// already the default type which we start to build. Only supported types
	// are here.
	InstancesList = []string{
		"t2.small",
		"t2.medium",
		"m3.medium",
		"c3.large",
		"m3.large",
		"c3.xlarge",
	}
)

type BuildData struct {
	// This is passed directly to goamz to create the final instance
	EC2Data *ec2.RunInstances
	KiteId  string
}

type Build struct {
	amazon        *amazon.AmazonClient
	machine       *protocol.Machine
	provider      *Provider
	start, finish int
	log           logging.Logger
}

// normalize returns the normalized step according to the initial start and finish
// values. i.e for a start,finish pair of (10,90) percentages of
// 0,15,20,50,80,100 will be according to the function: 10,18,26,50,74,90
func (b *Build) normalize(percentage int) int {
	base := b.finish - b.start
	step := float64(base) * (float64(percentage) / 100)
	normalized := float64(b.start) + step
	return int(normalized)

}

func (p *Provider) Build(m *protocol.Machine) (resArt *protocol.Artifact, err error) {
	a, err := p.NewClient(m)
	if err != nil {
		return nil, err
	}

	b := &Build{
		amazon:   a,
		machine:  m,
		provider: p,
		start:    10,
		finish:   90,
		log:      p.Log,
	}

	return b.run()
}

func (b *Build) run() (resArt *protocol.Artifact, err error) {
	b.amazon.Push("Generating and fetching build data", b.normalize(10), machinestate.Building)
	buildData, err := b.buildData()
	if err != nil {
		return nil, err
	}

	if err := b.checkLimits(buildData); err != nil {
		return nil, err
	}

	buildArtifact, err := b.create(buildData)
	if err != nil {
		return nil, err
	}

	// cleanup build if something goes wrong here
	defer func() {
		if err != nil {
			b.log.Warning("Cleaning up instance by terminating instance: %s. Error was: %s",
				buildArtifact.InstanceId, err)

			if _, err := b.amazon.Client.TerminateInstances([]string{buildArtifact.InstanceId}); err != nil {
				b.log.Warning("Cleaning up instance '%s' failed: %v", buildArtifact.InstanceId, err)
			}
		}
	}()

	if err := b.addDomainAndTags(buildArtifact); err != nil {
		return nil, err
	}

	checkKiteFunc := func() error {
		query := kiteprotocol.Kite{ID: buildData.KiteId}
		buildArtifact.KiteQuery = query.String()

		b.amazon.Push("Checking connectivity", b.normalize(75), machinestate.Building)

		b.log.Debug("[%s] Connecting to remote Klient instance", b.machine.Id)
		if b.provider.IsKlientReady(query.String()) {
			b.log.Debug("[%s] klient is ready.", b.machine.Id)
		} else {
			b.log.Warning("[%s] klient is not ready. I couldn't connect to it.", b.machine.Id)
		}

		return nil
	}

	steps := []func() error{
		checkKiteFunc,
	}

	for i, fn := range steps {
		fmt.Printf("Building step %d \n", i)
		if err := fn(); err != nil {
			return nil, err
		}
	}

	return buildArtifact, nil
}

func (b *Build) addDomainAndTags(buildArtifact *protocol.Artifact) error {
	// this can happen when an Info method is called on a terminated instance.
	// This updates the DB records with the name that EC2 gives us, which is a
	// "terminated-instance"
	instanceName := b.machine.Builder["instanceName"].(string)
	if instanceName == "terminated-instance" {
		instanceName = "user-" + b.machine.Username + "-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
		b.log.Debug("[%s] Instance name is an artifact (terminated), changing to %s", b.machine.Id, instanceName)
	}

	b.amazon.Push("Updating/Creating domain", b.normalize(70), machinestate.Building)
	if err := b.provider.UpdateDomain(buildArtifact.IpAddress, b.machine.Domain.Name, b.machine.Username); err != nil {
		return err
	}

	b.amazon.Push("Updating domain aliases", b.normalize(72), machinestate.Building)
	domains, err := b.provider.DomainStorage.GetByMachine(b.machine.Id)
	if err != nil {
		b.log.Error("[%s] fetching domains for setting err: %s", b.machine.Id, err.Error())
	}

	for _, domain := range domains {
		if err := b.provider.UpdateDomain(buildArtifact.IpAddress, domain.Name, b.machine.Username); err != nil {
			b.log.Error("[%s] couldn't update machine domain: %s", b.machine.Id, err.Error())
		}
	}

	buildArtifact.InstanceName = instanceName
	buildArtifact.MachineId = b.machine.Id
	buildArtifact.DomainName = b.machine.Domain.Name

	tags := []ec2.Tag{
		{Key: "Name", Value: buildArtifact.InstanceName},
		{Key: "koding-user", Value: b.machine.Username},
		{Key: "koding-env", Value: b.provider.Kite.Config.Environment},
		{Key: "koding-machineId", Value: b.machine.Id},
		{Key: "koding-domain", Value: b.machine.Domain.Name},
	}

	b.log.Debug("[%s] Adding user tags %v", b.machine.Id, tags)
	if err := b.amazon.AddTags(buildArtifact.InstanceId, tags); err != nil {
		b.log.Error("[%s] Adding tags failed: %v", b.machine.Id, err)
	}

	return nil
}

// checkLimits checks whether the given buildData is valid to be used to create a new instance
func (b *Build) checkLimits(buildData *BuildData) error {
	// Check for total machine allowance
	checker, err := b.provider.PlanChecker(b.machine)
	if err != nil {
		return err
	}

	if err := checker.Total(); err != nil {
		return err
	}

	if err := checker.AlwaysOn(); err != nil {
		return err
	}

	b.log.Debug("[%s] Check if user is allowed to create instance type %s",
		b.machine.Id, buildData.EC2Data.InstanceType)

	// check if the user is egligible to create a vm with this instance type
	if err := checker.AllowedInstances(instances[buildData.EC2Data.InstanceType]); err != nil {
		b.log.Critical("[%s] Instance type (%s) is not allowed. This shouldn't happen. Fallback to t2.micro",
			b.machine.Id, buildData.EC2Data.InstanceType)
		buildData.EC2Data.InstanceType = T2Micro.String()
	}

	// check if the user is egligible to create a vm with this size
	if err := checker.Storage(int(buildData.EC2Data.BlockDevices[0].VolumeSize)); err != nil {
		return err
	}

	return nil
}

func (b *Build) create(buildData *BuildData) (*protocol.Artifact, error) {
	// build our instance in a normal way
	buildArtifact, err := b.amazon.Build(buildData.EC2Data, b.normalize(45), b.normalize(60))
	if err == nil {
		return buildArtifact, nil
	}

	// check if the error is a 'InsufficientInstanceCapacity" error or
	// "InstanceLimitExceeded, if not return back because it's not a
	// resource or capacity problem.
	if !isCapacityError(err) {
		return nil, err
	}

	b.log.Error("[%s] IMPORTANT: %s", b.machine.Id, err)

	// now lets to some fallback mechanisms to avoid the capacity errors.
	// 1. Try to use a different zone
	zoneFunc := func() (*protocol.Artifact, error) {
		zones, err := b.provider.EC2Clients.Zones(b.amazon.Client.Region.Name)
		if err != nil {
			return nil, fmt.Errorf("couldn't fetch availability zones: %s", err)

		}

		b.log.Debug("[%s] Fallback: Searching for a zone that has capacity amongst zones: %v", b.machine.Id, zones)
		for _, zone := range zones {
			if zone == buildData.EC2Data.AvailZone {
				// skip it because that's one is causing problems and doesn't have any capacity
				continue
			}

			subnets, err := b.amazon.SubnetsWithTag(DefaultKloudKeyName)
			if err != nil {
				return nil, err
			}

			subnet, err := subnets.AvailabilityZone(zone)
			if err != nil {
				continue // shouldn't be happen, but let be safe
			}

			group, err := b.amazon.SecurityGroupFromVPC(subnet.VpcId, DefaultKloudKeyName)
			if err != nil {
				return nil, err
			}

			// add now our security group
			buildData.EC2Data.SecurityGroups = []ec2.SecurityGroup{{Id: group.Id}}
			buildData.EC2Data.AvailZone = zone
			buildData.EC2Data.SubnetId = subnet.SubnetId

			b.log.Warning("[%s] Building again by using availability zone: %s and subnet %s.",
				b.machine.Id, zone, subnet.SubnetId)

			buildArtifact, err := b.amazon.Build(buildData.EC2Data, b.normalize(60), b.normalize(70))
			if err == nil {
				return buildArtifact, nil
			}

			if isCapacityError(err) {
				// if there is no capacity we are going to use the next one
				b.log.Warning("[%s] Build failed on availability zone '%s' due to AWS capacity problems. Trying another region.",
					b.machine.Id, zone)
				continue
			}

			return nil, err
		}

		return nil, errors.New("no other zones are available")
	}

	// 2. Try to use another instance
	// TODO: do not choose an instance lower than the current user
	// instance. Currently all we give is t2.micro, however it if the user
	// has a t2.medium, we'll give them a t2.small if there is no capacity,
	// which needs to be fixed in the near future.
	instanceFunc := func() (*protocol.Artifact, error) {
		for _, instanceType := range InstancesList {
			b.log.Warning("[%s] Fallback: building again with using instance: %s instead of %s.",
				b.machine.Id, instanceType, buildData.EC2Data.InstanceType)

			buildData.EC2Data.InstanceType = instanceType

			buildArtifact, err := b.amazon.Build(buildData.EC2Data, b.normalize(60), b.normalize(70))
			if err == nil {
				return buildArtifact, nil // we are finished!
			}

			if isCapacityError(err) {
				continue // pick up the next one
			}

			b.log.Critical("[%s] Fallback didn't work for instances: %s", b.machine.Id, err)
			break // break if it's something else
		}

		return nil, errors.New("no other instances are available")
	}

	// We are going to to try each step and for each step if we get
	// "InsufficentInstanceCapacity" error we move to the next one.
	for _, fn := range []func() (*protocol.Artifact, error){zoneFunc, instanceFunc} {
		buildArtifact, err := fn()
		if err != nil {
			b.log.Error("Build failed. Moving to next fallback step: %s", err)
			continue // pick up the next function
		}

		return buildArtifact, nil
	}

	return nil, errors.New("build reached the end. all fallback mechanism steps failed.")
}

// buildData returns all necessary data that is needed to build a machine.
func (b *Build) buildData() (*BuildData, error) {
	// get all subnets belonging to Kloud
	subnets, err := b.amazon.SubnetsWithTag(DefaultKloudKeyName)
	if err != nil {
		return nil, err
	}

	// sort and get the lowest
	subnet := subnets.WithMostIps()

	group, err := b.amazon.SecurityGroupFromVPC(subnet.VpcId, DefaultKloudKeyName)
	if err != nil {
		return nil, err
	}

	image, err := b.amazon.ImageByTag(DefaultCustomAMITag)
	if err != nil {
		return nil, err
	}

	device := image.BlockDevices[0]

	storageSize := 3 // default AMI 3GB size
	if b.amazon.Builder.StorageSize != 0 && b.amazon.Builder.StorageSize > 3 {
		storageSize = b.amazon.Builder.StorageSize
	}

	// Increase storage if it's passed to us, otherwise the default 3GB is
	// created already with the default AMI
	blockDeviceMapping := ec2.BlockDeviceMapping{
		DeviceName:          device.DeviceName,
		VirtualName:         device.VirtualName,
		SnapshotId:          device.SnapshotId,
		VolumeType:          "standard", // Use magnetic storage because it is cheaper
		VolumeSize:          int64(storageSize),
		DeleteOnTermination: true,
		Encrypted:           false,
	}

	b.log.Debug("Using subnet: '%s', zone: '%s', sg: '%s'. Subnet has %d available IPs",
		subnet.SubnetId, subnet.AvailabilityZone, group.Id, subnet.AvailableIpAddressCount)

	if b.amazon.Builder.InstanceType == "" {
		b.log.Critical("Instance type is empty. This shouldn't happen. Fallback to t2.micro")
		b.amazon.Builder.InstanceType = T2Micro.String()
	}

	kiteUUID, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}

	kiteId := kiteUUID.String()

	userData, err := b.userData(kiteId)
	if err != nil {
		return nil, err
	}

	ec2Data := &ec2.RunInstances{
		ImageId:                  image.Id,
		MinCount:                 1,
		MaxCount:                 1,
		KeyName:                  b.provider.KeyName,
		InstanceType:             b.amazon.Builder.InstanceType,
		AssociatePublicIpAddress: true,
		SubnetId:                 subnet.SubnetId,
		SecurityGroups:           []ec2.SecurityGroup{{Id: group.Id}},
		AvailZone:                subnet.AvailabilityZone,
		BlockDevices:             []ec2.BlockDeviceMapping{blockDeviceMapping},
		UserData:                 userData,
	}

	return &BuildData{
		EC2Data: ec2Data,
		KiteId:  kiteId,
	}, nil
}

func (b *Build) userData(kiteId string) ([]byte, error) {
	kiteKey, err := b.createKey(b.machine.Username, kiteId)
	if err != nil {
		return nil, err
	}

	latestKlientPath, err := b.provider.Bucket.LatestDeb()
	if err != nil {
		return nil, err
	}

	latestKlientUrl := b.provider.Bucket.URL(latestKlientPath)

	// Use cloud-init for initial configuration of the VM
	cloudInitConfig := &CloudInitConfig{
		Username:        b.machine.Username,
		UserDomain:      b.machine.Domain.Name,
		Hostname:        b.machine.Username, // no typo here. hostname = username
		KiteKey:         kiteKey,
		LatestKlientURL: latestKlientUrl,
		ApachePort:      DefaultApachePort,
		KitePort:        DefaultKitePort,
	}

	// check if the user has some keys
	if keyData, ok := b.machine.Builder["user_ssh_keys"]; ok {
		if keys, ok := keyData.([]string); ok && len(keys) > 0 {
			for _, key := range keys {
				// validate the public keys
				_, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
				if err != nil {
					b.log.Error(`User (%s) has an invalid public SSH key.
							Not adding it to the authorized keys.
							Key: %s. Err: %v`, b.machine.Username, key, err)
					continue
				}
				cloudInitConfig.UserSSHKeys = append(cloudInitConfig.UserSSHKeys, key)
			}
		}
	}

	var userdata bytes.Buffer
	err = cloudInitTemplate.Funcs(funcMap).Execute(&userdata, *cloudInitConfig)
	if err != nil {
		return nil, err
	}

	// validate the userdata first before sending
	if err = yaml.Unmarshal(userdata.Bytes(), struct{}{}); err != nil {
		// write to temporary file so we can see the yaml file that is not
		// formatted in a good way.
		f, err := ioutil.TempFile("", "kloud-cloudinit")
		if err == nil {
			if _, err := f.WriteString(userdata.String()); err != nil {
				b.log.Error("Cloudinit temporary field couldn't be written %v", err)
			}
		}

		b.log.Error("Cloudinit template is not a valid YAML file: %v. YAML file path: %s", err,
			f.Name())
		return nil, errors.New("Cloudinit template is not a valid YAML file.")
	}

	return userdata.Bytes(), nil

}

// CreateKey signs a new key and returns the token back
func (b *Build) createKey(username, kiteId string) (string, error) {
	if username == "" {
		return "", kloud.NewError(kloud.ErrSignUsernameEmpty)
	}

	if b.provider.KontrolURL == "" {
		return "", kloud.NewError(kloud.ErrSignKontrolURLEmpty)
	}

	if b.provider.KontrolPrivateKey == "" {
		return "", kloud.NewError(kloud.ErrSignPrivateKeyEmpty)
	}

	if b.provider.KontrolPublicKey == "" {
		return "", kloud.NewError(kloud.ErrSignPublicKeyEmpty)
	}

	token := jwt.New(jwt.GetSigningMethod("RS256"))

	token.Claims = map[string]interface{}{
		"iss":        "koding",                                       // Issuer, should be the same username as kontrol
		"sub":        username,                                       // Subject
		"iat":        time.Now().UTC().Unix(),                        // Issued At
		"jti":        kiteId,                                         // JWT ID
		"kontrolURL": b.provider.KontrolURL,                          // Kontrol URL
		"kontrolKey": strings.TrimSpace(b.provider.KontrolPublicKey), // Public key of kontrol
	}

	return token.SignedString([]byte(b.provider.KontrolPrivateKey))
}
