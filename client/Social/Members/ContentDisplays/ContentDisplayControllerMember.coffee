class ContentDisplayControllerMember extends KDViewController

  neo4jFacets = [
    "JLink"
    "JBlogPost"
    "JTutorial"
    "JStatusUpdate"
    "JComment"
    "JOpinion"
    "JDiscussion"
    "JCodeSnip"
  ]

  constructor:(options={}, data)->

    {@revivedContentDisplay} = KD.singleton('display')

    options = $.extend
      view : mainView = new KDView
        cssClass : 'member content-display'
        domId    : 'member-contentdisplay'  unless @revivedContentDisplay
        type     : 'profile'
    , options
    super options, data

  loadView:(mainView)->
    member = @getData()
    {lazy} = mainView

    # FIX THIS GG

    # @updateWidget = new ActivityUpdateWidget
    #   cssClass: 'activity-update-widget-wrapper-folded'

    # @updateWidgetController = new ActivityUpdateWidgetController
    #   view : @updateWidget

    # mainView.addSubView @updateWidget
    # cdc = KD.singleton('display')
    # if not cdc._updateController
    #   cdc._updateController = {}
    #   cdc._updateController.updateWidget = new ActivityUpdateWidget
    #     cssClass: 'activity-update-widget-wrapper-folded'

    #   cdc._updateController.updateWidgetController = new ActivityUpdateWidgetController
    #     view : cdc._updateController.updateWidget

    # mainView.addSubView cdc._updateController.updateWidget

    @addProfileView member

    if lazy
      viewClass = if KD.isLoggedIn() then KDCustomHTMLView else HomeLoginBar
      mainView.addSubView @homeLoginBar = new viewClass
        domId : "home-login-bar"
      @homeLoginBar.hide()  if KD.isLoggedIn()

    @addActivityView member

  addProfileView:(member)->
    options      =
      cssClass   : "profilearea profile-right-block clearfix"
      domId      : 'profilearea' unless @revivedContentDisplay
      delegate   : @getView()

    if KD.isMine member
      options.cssClass = KD.utils.curry "own-profile", options.cssClass
    else
      options.bind = "mouseenter" unless KD.isMine member

    return @getView().addSubView memberProfile = new ProfileView options, member

  # mouseEnterOnFeed:->
  #
  #   clearTimeout @intentTimer
  #   @intentTimer = setTimeout =>
  #     @getView().$('.profilearea').css "overflow", "hidden"
  #     @getView().setClass "small-header"
  #     @utils.wait 300,=>
  #       KD.getSingleton('windowController').notifyWindowResizeListeners()
  #   , 500
  #
  # mouseEnterOnHeader:->
  #
  #   clearTimeout @intentTimer
  #   @intentTimer = setTimeout =>
  #     @getView().unsetClass "small-header"
  #     @utils.wait 300,=>
  #       KD.getSingleton('windowController').notifyWindowResizeListeners()
  #       @getView().$('.profilearea').css "overflow", "visible"
  #   , 500

  createFilter:(title, account, facets)->
    filter =
      title             : title
      dataSource        : (selector, options, callback)=>
        options.originId = account.getId()
        options.facet   = facets
        KD.getSingleton("appManager").tell 'Activity', 'fetchActivitiesProfilePage', options, callback
    return filter

  addActivityView:(account)->
    @getView().$('div.lazy').remove()

    KD.getSingleton("appManager").tell 'Activity', 'feederBridge', {
      domId                 : 'members-feeder-split-view' unless @revivedContentDisplay
      itemClass             : ActivityListItemView
      listControllerClass   : ActivityListController
      listCssClass          : "activity-related"
      limitPerPage          : 8
      useHeaderNav          : yes
      delegate              : @getOption 'delegate'
      help                  :
        subtitle            : "Learn Personal feed"
        tooltip             :
          title             : "<p class=\"bigtwipsy\">This is the personal feed of a single Koding user.</p>"
          placement         : "above"
      filter                :
        everything          : @createFilter("Everything", account, 'Everything')
        statuses            : @createFilter("Status Updates", account, 'JStatusUpdate')
        codesnips           : @createFilter("Code Snippets", account, 'JCodeSnip')
        blogposts           : @createFilter("Blog Posts", account, 'JBlogPost')
        discussions         : @createFilter("Discussions", account, 'JDiscussion')
        tutorials           : @createFilter("Tutorials", account, 'JTutorial')
      sort                  :
        'likesCount'  :
          title             : "Most popular"
          direction         : -1
        'modifiedAt'        :
          title             : "Latest activity"
          direction         : -1
        'repliesCount':
          title             : "Most commented"
          direction         : -1
        # and more
    }, (controller)=>
      @feedController = controller
      @getView().addSubView controller.getView()
      @emit 'ready'
