// The admins' WebDAV screen: every account with a WebDAV folder, whether it
// may use WebDAV, its quota and how much it holds -- never what is in it.
//
// app.js calls WebDAVAdmin.init( me ) for admins. It stays hidden when the
// server says WebDAV is off.
const WebDAVAdmin = {
  me: null,

  async init( me ) {
    this.me = me;
    // A first look, to stay hidden while the server says WebDAV is off.
    try {
      await Api.listAdminWebDAV( 0 , { limit: 1 } );
    } catch ( error ) {
      if ( error.status === 404 ) return;
      throw error;
    }
    await this.refresh();
    Dom.show( Dom.get( "webdav-admin-panel" ) , true );
    Pager.sortable( "webdav-admin-table" , () => this.refresh() );
    Pager.search( "webdav-admin-table" , "webdav-admin-search" , () => this.refresh() );
  },

  async refresh() {
    const body = Dom.get( "webdav-admin-table" ).querySelector( "tbody" );
    await Pager.load( "webdav-admin-table" , "accounts" , ( offset ) => Api.listAdminWebDAV( offset , Pager.params( "webdav-admin-table" ) ) , function ( account ) {
      const quota = account.quota_bytes > 0 ? WebDAV.size( account.quota_bytes ) : I18n.get( "webdav_admin.no_limit" );
      body.appendChild( Dom.el( "tr" , { children: [
        Dom.el( "td" , { text: account.display_name || ( "#" + account.user_id ) } ),
        Dom.el( "td" , { class: "text-nowrap" , children: [
          Dom.el( "code" , { text: account.name } ),
          account.label ? Dom.el( "span" , { class: "small text-body-secondary ms-1" , text: account.label } ) : null,
          account.name_locked ? Dom.el( "i" , { class: "bi bi-lock ms-1 text-body-secondary" , attrs: { title: I18n.get( "webdav_admin.name_locked" ) , "aria-label": I18n.get( "webdav_admin.name_locked" ) , role: "img" } } ) : null,
        ].filter( Boolean ) } ),
        Dom.el( "td" , { children: [ Dom.badge( I18n.get( account.enabled ? "webdav_admin.access_on" : "webdav_admin.access_off" ) , account.enabled ? "success" : "secondary" ) ] } ),
        Dom.el( "td" , { class: "text-nowrap" , text: WebDAV.size( account.used_bytes ) } ),
        Dom.el( "td" , { class: "text-nowrap" , children: [
          Dom.el( "span" , { text: quota } ),
          account.quota_set ? null : Dom.el( "span" , { class: "small text-body-secondary ms-1" , text: I18n.get( "webdav_admin.default_quota" ) } ),
        ].filter( Boolean ) } ),
        Dom.el( "td" , { children: [ Dom.rowActions( account.name , [
          WebDAVAdmin.accessButton( account ),
          WebDAVAdmin.lockButton( account ),
          WebDAVAdmin.quotaButton( account ),
        ] ) ] } ),
      ] } ) );
    } );
  },

  // Switching access is undoable: it only changes a flag.
  accessButton( account ) {
    return Dom.el( "button" , {
      class: account.enabled ? "btn btn-sm btn-outline-danger" : "btn btn-sm btn-outline-secondary",
      text: I18n.get( account.enabled ? "webdav_admin.turn_off" : "webdav_admin.turn_on" ),
      on: { click: async function () {
        const set = async ( on ) => { await Api.setAdminWebDAV( account.id , { enabled: on } ); await WebDAVAdmin.refresh(); };
        try {
          await Undo.run( {
            label: I18n.get( "undo.webdav_access" ),
            toast: true,
            do: () => set( !account.enabled ),
            undo: () => set( account.enabled ),
          } );
        } catch ( error ) { App.showError( error.message ); }
      } },
    } );
  },

  // Whether they may change their own WebDAV name. Undoable: a flag again.
  lockButton( account ) {
    return Dom.el( "button" , {
      class: "btn btn-sm btn-outline-secondary",
      text: I18n.get( account.name_locked ? "webdav_admin.unlock_name" : "webdav_admin.lock_name" ),
      on: { click: async function () {
        const set = async ( locked ) => { await Api.setAdminWebDAV( account.id , { name_locked: locked } ); await WebDAVAdmin.refresh(); };
        try {
          await Undo.run( {
            label: I18n.get( "undo.webdav_name_lock" ),
            toast: true,
            do: () => set( !account.name_locked ),
            undo: () => set( account.name_locked ),
          } );
        } catch ( error ) { App.showError( error.message ); }
      } },
    } );
  },

  quotaButton( account ) {
    return Dom.el( "button" , {
      class: "btn btn-sm btn-outline-secondary",
      text: I18n.get( "webdav_admin.quota_button" ),
      on: { click: async function () {
        const previous = account.quota_set ? { quota_gb: Math.round( account.quota_bytes / 1073741824 ) } : { default_quota: true };
        const saved = await Dom.form( {
          title: I18n.get( "webdav_admin.quota_heading" ).replace( "{name}" , () => account.display_name || account.name ),
          fields: [ {
            name: "quota_gb",
            label: I18n.get( "webdav_admin.quota_label" ),
            type: "number",
            min: 0,
            max: 1048576,
            value: account.quota_set ? String( Math.round( account.quota_bytes / 1073741824 ) ) : "",
            help: I18n.get( "webdav_admin.quota_help" ),
          } ],
          save: async ( values ) => {
            const next = values.quota_gb === null
              ? { default_quota: true }
              : { quota_gb: Number( values.quota_gb ) };
            await Api.setAdminWebDAV( account.id , next );
            Undo.record( {
              label: I18n.get( "undo.webdav_quota" ),
              toast: true,
              undo: async () => { await Api.setAdminWebDAV( account.id , previous ); await WebDAVAdmin.refresh(); },
              redo: async () => { await Api.setAdminWebDAV( account.id , next ); await WebDAVAdmin.refresh(); },
            } );
          },
        } );
        if ( saved === null ) return;
        try {
          await WebDAVAdmin.refresh();
        } catch ( error ) { App.showError( error.message ); }
      } },
    } );
  },
};
