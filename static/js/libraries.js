// WebDAV libraries: an account may have several, each its own folder, name,
// app passwords, quota and metadata library (say one per Zotero profile).
//
// One of them is "picked" at a time, and every WebDAV and library call goes to
// it (Api.library). The pick is remembered per browser. A picker sits at the
// top of each screen that works on one library, shown once there is more than
// one; changing it reloads the page, so every screen starts clean on the new
// library. The Libraries card on the WebDAV screen lists them, makes new ones,
// labels and deletes them.
//
// WebDAV.init calls Libraries.restore before its first call, and
// Libraries.render with every state it gets.
const Libraries = {
  storageKey: "webdav-library",
  list: [],
  max: 0,
  wired: false,

  // The remembered pick, before anything asks the server.
  restore() {
    try {
      Api.library = window.localStorage.getItem( this.storageKey ) || "";
    } catch ( error ) {
      Api.library = "";
    }
  },

  // Forget a pick that no longer works (the library was deleted elsewhere).
  forget() {
    Api.library = "";
    try { window.localStorage.removeItem( this.storageKey ); } catch ( error ) { /* nothing to forget */ }
  },

  pick( name ) {
    try {
      if ( name ) window.localStorage.setItem( this.storageKey , name );
      else window.localStorage.removeItem( this.storageKey );
    } catch ( error ) { /* a private window: the pick lasts this page only */ }
    Api.library = name;
    window.location.reload();
  },

  // The name to send for a library: "" for the first, which every call
  // reaches without one.
  nameFor( library ) {
    return library.primary ? "" : library.name;
  },

  render( state ) {
    this.list = state.libraries || [];
    this.max = state.max_libraries || 0;
    this.wire();
    this.renderPickers();
    this.renderCard( state );
  },

  wire() {
    if ( this.wired ) return;
    this.wired = true;
    Dom.all( "[data-library-picker] select" ).forEach( ( select ) => {
      select.addEventListener( "change" , () => this.pick( select.value ) );
    } );
    Dom.onSubmit( Dom.get( "libraries-add-form" ) , this.onAdd.bind( this ) );
  },

  renderPickers() {
    const many = this.list.length > 1;
    Dom.all( "[data-library-picker]" ).forEach( ( row ) => {
      Dom.show( row , many );
      const select = row.querySelector( "select" );
      Dom.clear( select );
      this.list.forEach( ( library ) => {
        const option = Dom.el( "option" , { text: library.label ? library.name + " (" + library.label + ")" : library.name } );
        option.value = this.nameFor( library );
        option.selected = option.value === Api.library;
        select.appendChild( option );
      } );
    } );
  },

  // The card shows once the first library has its name: before that the
  // Connect card's claim form is the way to make it.
  renderCard( state ) {
    const card = Dom.get( "libraries-card" );
    Dom.show( card , this.list.length > 0 );
    if ( this.list.length === 0 ) return;
    const full = this.max > 0 && this.list.length >= this.max;
    Dom.show( Dom.get( "libraries-add-form" ) , !full );
    Dom.show( Dom.get( "libraries-full" ) , full );
    Dom.text( Dom.get( "libraries-full" ) , I18n.get( "libraries.full" ).replace( "{max}" , () => String( this.max ) ) );

    const body = Dom.get( "libraries-table" ).querySelector( "tbody" );
    Dom.clear( body );
    this.list.forEach( ( library ) => {
      const picked = this.nameFor( library ) === Api.library;
      const name = Dom.el( "td" , { children: [
        Dom.el( "code" , { text: library.name } ),
        library.primary ? this.tag( "libraries.primary" , "secondary" ) : null,
        picked ? this.tag( "libraries.picked" , "primary" ) : null,
        library.enabled ? null : this.tag( "libraries.off" , "warning" ),
      ].filter( Boolean ) } );
      body.appendChild( Dom.el( "tr" , { children: [
        name,
        Dom.el( "td" , { children: [ Dom.editable( library.label , {
          label: I18n.get( "libraries.table_label" ),
          placeholder: I18n.get( "libraries.no_label" ),
          optional: true,
          maxLength: 60,
          save: async ( label ) => {
            const saved = await Api.labelWebDAVLibrary( library.id , label );
            library.label = saved.label || "";
            this.renderPickers();
            return library.label;
          },
        } ) ] } ),
        Dom.el( "td" , { class: "d-none d-md-table-cell text-nowrap" , text: this.usage( library ) } ),
        Dom.el( "td" , { children: [ Dom.rowActions( library.name , [
          picked ? null : Dom.el( "button" , {
            class: "btn btn-sm btn-outline-primary",
            text: I18n.get( "libraries.open_button" ),
            on: { click: () => this.pick( this.nameFor( library ) ) },
          } ),
          library.primary ? null : this.deleteButton( library , picked ),
        ] ) ] } ),
      ] } ) );
    } );
  },

  tag( key , tone ) {
    const badge = Dom.badge( I18n.get( key ) , tone );
    badge.classList.add( "ms-1" );
    return badge;
  },

  usage( library ) {
    const used = WebDAV.size( library.used_bytes );
    if ( library.quota_bytes > 0 ) return used + " / " + WebDAV.size( library.quota_bytes );
    return used;
  },

  // There is no undo: the files go with the next upkeep run.
  deleteButton( library , picked ) {
    return Dom.el( "button" , {
      class: "btn btn-sm btn-outline-danger",
      text: I18n.get( "libraries.delete_button" ),
      on: { click: async () => {
        const question = I18n.get( "libraries.delete_confirm" ).replace( "{name}" , () => library.name );
        if ( !( await Dom.confirm( question ) ) ) return;
        try {
          await Api.deleteWebDAVLibrary( library.id );
          Dom.toast( I18n.get( "libraries.deleted" ) , "ok" );
          if ( picked ) {
            this.pick( "" );
            return;
          }
          await WebDAV.reload();
        } catch ( error ) {
          App.showError( error.message );
        }
      } },
    } );
  },

  async onAdd( event ) {
    event.preventDefault();
    const nameField = Dom.get( "libraries-add-name" );
    const name = nameField.value.trim().toLowerCase();
    const label = Dom.get( "libraries-add-label" ).value.trim();
    try {
      await Api.createWebDAVLibrary( name , label );
      nameField.value = "";
      Dom.get( "libraries-add-label" ).value = "";
      Dom.toast( I18n.get( "libraries.created" ).replace( "{name}" , () => name ) , "ok" );
      await WebDAV.reload();
    } catch ( error ) {
      Dom.fieldError( nameField , error.message );
    }
  },
};
