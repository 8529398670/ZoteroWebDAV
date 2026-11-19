// The WebDAV screen: what to copy into Zotero, laid out like Zotero's own
// File Syncing form, how full your folder is, and the app passwords Zotero
// signs in with. The folder itself is the Files screen (files.js) and what was
// deleted from it the Trash screen (trash.js).
//
// app.js calls WebDAV.init( me ) once it knows who is signed in. The server
// answers 404 for every WebDAV route when webdav.enabled is off, and then this
// screen, Files, Trash and Library all stay hidden.
const WebDAV = {
  me: null,
  state: null,
  // A rename is waiting on its question.
  renaming: false,

  async init( me ) {
    this.me = me;
    let state;
    try {
      state = await Api.webdav();
    } catch ( error ) {
      if ( error.status === 404 ) return;
      throw error;
    }
    Dom.show( Dom.get( "webdav-panel" ) , true );
    Library.init( me );

    Dom.onSubmit( Dom.get( "webdav-claim-form" ) , this.onClaim.bind( this ) );
    Dom.onSubmit( Dom.get( "webdav-rename-form" ) , this.onRename.bind( this ) );
    Dom.onPress( Dom.get( "webdav-make-password" ) , this.onMakePassword.bind( this ) );
    Dom.onPress( Dom.get( "webdav-random-name" ) , this.onRandomName.bind( this ) );
    // The username is edited in place: Enter or leaving the field renames,
    // Escape puts the current name back.
    const username = Dom.get( "webdav-username" );
    username.addEventListener( "blur" , () => {
      // The question a rename asks takes the focus too: not a second rename.
      if ( this.renaming ) return;
      if ( this.state && this.state.account && username.value.trim().toLowerCase() !== this.state.account.name ) Dom.get( "webdav-rename-form" ).requestSubmit();
    } );
    username.addEventListener( "keydown" , ( event ) => {
      if ( event.key === "Escape" && this.state && this.state.account ) username.value = this.state.account.name;
    } );
    Dom.get( "webdav-claim-random" ).addEventListener( "click" , () => {
      Dom.get( "webdav-claim-name" ).value = this.randomName();
    } );
    Dom.get( "webdav-copy-url" ).addEventListener( "click" , () => Dom.copy( Dom.get( "webdav-host-path" ) ) );
    Dom.get( "webdav-copy-username" ).addEventListener( "click" , () => Dom.copy( Dom.get( "webdav-username" ) ) );
    Dom.get( "webdav-copy-password" ).addEventListener( "click" , () => Dom.copy( Dom.get( "webdav-password-value" ) ) );
    // An admin may have switched your access or locked your name since the
    // page loaded, so coming back to this screen asks again.
    window.addEventListener( "hashchange" , () => {
      if ( window.location.hash === "#webdav" ) this.reload().catch( ( error ) => App.showError( error.message ) );
    } );
    await this.render( state );
  },

  async reload() {
    await this.render( await Api.webdav() );
  },

  // Everything that follows from the account: the claim form or the Zotero
  // form, usage, and the screens that need a name first.
  async render( state ) {
    this.state = state;
    const account = state.account;
    Dom.show( Dom.get( "webdav-claim" ) , account === null );
    Dom.show( Dom.get( "webdav-connect" ) , account !== null );
    Dom.show( Dom.get( "webdav-passwords-card" ) , account !== null );
    // Nav.init runs after every screen's init, so this decides the sidebar too.
    Dom.show( Dom.get( "files-panel" ) , account !== null );
    Dom.show( Dom.get( "trash-panel" ) , account !== null );
    if ( account === null ) {
      const field = Dom.get( "webdav-claim-name" );
      if ( field.value === "" ) field.value = state.suggested_name || "";
      Dom.text( Dom.get( "webdav-usage-text" ) , I18n.get( "webdav.usage_none" ) );
      this.usageBar( 0 , 0 );
      return;
    }
    const username = Dom.get( "webdav-username" );
    const scheme = Dom.get( "webdav-scheme" );
    Dom.swap( scheme , Dom.el( "option" , { text: state.scheme , attrs: { value: state.scheme } } ) );
    Dom.get( "webdav-host-path" ).value = state.host_path;
    username.value = account.name;
    // An admin can stop you changing it: then it is only there to copy.
    username.readOnly = account.name_locked;
    Dom.show( Dom.get( "webdav-random-name" ) , !account.name_locked );
    Dom.show( Dom.get( "webdav-name-locked" ) , account.name_locked );
    Dom.show( Dom.get( "webdav-off-note" ) , !account.enabled );
    Dom.show( Dom.get( "webdav-encrypted-note" ) , state.encrypt_at_rest );
    Dom.show( Dom.get( "webdav-plain-note" ) , !state.encrypt_at_rest );
    this.showUsage( account );
    Files.init();
    await Promise.all( [ this.refreshPasswords() , Trash.init() ] );
  },

  showUsage( account ) {
    const used = WebDAV.size( account.used_bytes );
    const text = account.quota_bytes > 0
      ? I18n.get( "webdav.usage_of" ).replace( "{used}" , () => used ).replace( "{quota}" , () => WebDAV.size( account.quota_bytes ) )
      : I18n.get( "webdav.usage_unlimited" ).replace( "{used}" , () => used );
    Dom.text( Dom.get( "webdav-usage-text" ) , text );
    this.usageBar( account.used_bytes , account.quota_bytes );
  },

  usageBar( used , quota ) {
    const percent = quota > 0 ? Math.min( 100 , Math.round( used * 100 / quota ) ) : 0;
    const bar = Dom.get( "webdav-usage-bar" );
    bar.style.width = percent + "%";
    bar.className = "progress-bar" + ( percent >= 90 ? " bg-danger" : percent >= 75 ? " bg-warning" : "" );
    Dom.get( "webdav-usage-bar-holder" ).setAttribute( "aria-valuenow" , String( percent ) );
  },

  async refreshUsage() {
    const state = await Api.webdav();
    this.state = state;
    if ( state.account ) this.showUsage( state.account );
  },

  // Bytes for people: 1.4 MB, in the browser's own number format.
  size( bytes ) {
    const units = [ "B" , "KB" , "MB" , "GB" , "TB" ];
    let value = bytes;
    let unit = 0;
    while ( value >= 1024 && unit < units.length - 1 ) {
      value /= 1024;
      unit += 1;
    }
    const digits = unit === 0 || value >= 100 ? 0 : 1;
    return value.toLocaleString( undefined , { maximumFractionDigits: digits } ) + " " + units[ unit ];
  },

  // A username nobody would guess from yours: twelve random lowercase
  // letters and digits. It hides nothing from anyone holding the app
  // password, but it keeps your name out of the URL.
  randomName() {
    const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789";
    const bytes = crypto.getRandomValues( new Uint8Array( 12 ) );
    return Array.from( bytes , ( byte ) => alphabet[ byte % alphabet.length ] ).join( "" );
  },

  async onClaim( event ) {
    event.preventDefault();
    const name = Dom.get( "webdav-claim-name" ).value.trim().toLowerCase();
    try {
      await this.render( await Api.setWebDAVName( name ) );
      Dom.toast( I18n.get( "webdav.claimed" ) , "ok" );
    } catch ( error ) {
      Dom.fieldError( Dom.get( "webdav-claim-name" ) , error.message );
    }
  },

  async onRename( event ) {
    event.preventDefault();
    const field = Dom.get( "webdav-username" );
    const name = field.value.trim().toLowerCase();
    if ( this.renaming || this.state.account.name_locked || name === this.state.account.name ) return;
    this.renaming = true;
    try {
      // A No puts the name back as it was.
      if ( !( await this.rename( name , I18n.get( "webdav.rename_confirm" ) ) ) ) field.value = this.state.account.name;
    } catch ( error ) {
      // Locked since the page loaded: show it, with the name put back.
      if ( error.status === 403 ) await this.reload();
      Dom.fieldError( field , error.message );
    } finally {
      this.renaming = false;
    }
  },

  async onRandomName() {
    try {
      await this.rename( this.randomName() , I18n.get( "webdav.random_name_confirm" ) );
    } catch ( error ) {
      if ( error.status === 403 ) await this.reload();
      App.showError( error.message );
    }
  },

  // A rename moves no file, but Zotero needs the new URL and username, so it
  // asks first; Ctrl+Z puts the old name back if it is still free. Answers
  // whether it happened.
  async rename( name , question ) {
    const previous = this.state.account.name;
    if ( !( await Dom.confirm( question ) ) ) return false;
    const to = async ( value ) => this.render( await Api.setWebDAVName( value ) );
    await Undo.run( {
      label: I18n.get( "undo.webdav_rename" ),
      toast: true,
      do: () => to( name ),
      undo: () => to( previous ),
    } );
    return true;
  },

  // The Zotero form's own button: a new app password, named for Zotero and
  // the day, straight into the Password field to copy.
  async onMakePassword() {
    try {
      const created = await Api.createWebDAVPassword( I18n.get( "webdav.password_default_name" ).replace( "{date}" , new Date().toLocaleDateString() ) , null );
      Dom.get( "webdav-password-value" ).value = created.credential;
      Dom.get( "webdav-copy-password" ).disabled = false;
      Dom.show( Dom.get( "webdav-password-once" ) , true );
      await this.refreshPasswords();
    } catch ( error ) {
      App.showError( error.message );
    }
  },

  // ------------------------------------------------------------------
  // app passwords
  // ------------------------------------------------------------------

  async refreshPasswords() {
    const body = Dom.get( "webdav-passwords-table" ).querySelector( "tbody" );
    await Pager.load( "webdav-passwords-table" , "passwords" , ( offset ) => Api.listWebDAVPasswords( offset ) , function ( password ) {
      body.appendChild( Dom.el( "tr" , { children: [
        Dom.el( "td" , { children: [
          Dom.el( "div" , { text: password.name } ),
          Dom.el( "div" , { class: "small text-body-secondary" , children: [ Dom.el( "span" , { text: I18n.get( "api_keys.created" ) + " " } ) , Dom.when( password.created_at ) ] } ),
        ] } ),
        Dom.el( "td" , { children: [ Keys.statusBadge( password ) ] } ),
        Dom.el( "td" , { children: [ Dom.when( password.last_used_at , I18n.get( "api_keys.never" ) ) ] } ),
        Dom.el( "td" , { class: "d-none d-md-table-cell" , children: [ Dom.when( password.expires_at , I18n.get( "api_keys.never" ) ) ] } ),
        Dom.el( "td" , { children: [ Dom.rowActions( password.name , [
          WebDAV.revokePasswordButton( password ),
          WebDAV.deletePasswordButton( password ),
        ] ) ] } ),
      ] } ) );
    } );
  },

  // Revoke is final, like an API key's: it is the "this leaked" button.
  revokePasswordButton( password ) {
    if ( !password.live ) return null;
    return Dom.el( "button" , {
      class: "btn btn-sm btn-outline-danger",
      text: I18n.get( "api_keys.revoke_button" ),
      on: { click: async function () {
        if ( !( await Dom.confirm( I18n.get( "webdav.password_revoke_confirm" ) ) ) ) return;
        try {
          await Api.revokeWebDAVPassword( password.id );
          await WebDAV.refreshPasswords();
        } catch ( error ) { App.showError( error.message ); }
      } },
    } );
  },

  // Delete stops it at once and can be undone for a few minutes.
  deletePasswordButton( password ) {
    return Dom.el( "button" , {
      class: "btn btn-sm btn-outline-danger",
      text: I18n.get( "api_keys.delete_button" ),
      on: { click: async function () {
        try {
          await Undo.run( {
            label: I18n.get( "undo.webdav_password_deleted" ),
            toast: true,
            do: async () => { await Api.deleteWebDAVPassword( password.id ); await WebDAV.refreshPasswords(); },
            undo: async () => { await Api.restoreWebDAVPassword( password.id ); await WebDAV.refreshPasswords(); },
          } );
        } catch ( error ) { App.showError( error.message ); }
      } },
    } );
  },
};
