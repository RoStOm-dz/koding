package cmd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/koding/kite/kitekey"
)

type List struct{}

func NewList() *List {
	return &List{}
}

func (*List) Definition() string {
	return "List installed kites"
}

func (*List) Exec(args []string) error {
	kites, err := getInstalledKites("")
	if err != nil {
		return err
	}

	for _, k := range kites {
		fmt.Println(k)
	}

	return nil
}

// getIntalledKites returns installed kites in .kd/kites folder.
// an empty argument returns all kites.
func getInstalledKites(kiteName string) ([]*InstalledKite, error) {
	kiteHome, err := kitekey.KiteHome()
	if err != nil {
		return nil, err
	}
	kitesPath := filepath.Join(kiteHome, "kites")

	domains, err := ioutil.ReadDir(kitesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	var installedKites []*InstalledKite // to be returned

	for _, domain := range domains {
		domainPath := filepath.Join(kitesPath, domain.Name())
		users, err := ioutil.ReadDir(domainPath)
		if err != nil {
			fmt.Println(err)
			continue
		}

		for _, user := range users {
			userPath := filepath.Join(domainPath, user.Name())
			repos, err := ioutil.ReadDir(userPath)
			if err != nil {
				fmt.Println(err)
				continue
			}

			for _, repo := range repos {
				repoPath := filepath.Join(userPath, repo.Name())
				versions, err := ioutil.ReadDir(repoPath)
				if err != nil {
					fmt.Println(err)
					continue
				}

				for _, version := range versions {
					versionPath := filepath.Join(repoPath, version.Name())
					binaryPath := filepath.Join(versionPath, "bin", strings.TrimSuffix(repo.Name(), ".kite"))
					_, err := os.Stat(binaryPath)
					if err != nil {
						fmt.Println(err)
						continue
					}

					installedKites = append(installedKites, NewInstalledKite(domain.Name(), user.Name(), repo.Name(), version.Name()))
				}
			}
		}
	}

	return installedKites, nil
}

type InstalledKite struct {
	Domain  string
	User    string
	Repo    string
	Version string
}

func NewInstalledKite(domain, user, repo, version string) *InstalledKite {
	return &InstalledKite{
		Domain:  domain,
		User:    user,
		Repo:    repo,
		Version: version,
	}
}

func (k *InstalledKite) String() string {
	return k.Domain + "/" + k.User + "/" + k.Repo + "/" + k.Version
}

// BinPath returns the path of the executable binary file.
func (k *InstalledKite) BinPath() string {
	return filepath.Join(k.Domain, k.User, k.Repo, k.Version, "bin", strings.TrimSuffix(k.Repo, ".kite"))
}
