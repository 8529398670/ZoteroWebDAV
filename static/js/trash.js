// The Trash screen: what was deleted on the Files screen, newest first, until
// it is put back or emptied after webdav.trash_days. Trash entries are whole
// paths, not a tree, so this stays a paged table.
//
// WebDAV.render calls Trash.init once there is a folder.
const Trash = {
  ready: false,

  async init() {
    if ( this.ready ) return;
    this.ready = true;
    await this.refresh();
  },

  async refresh() {
    const body = Dom.get( "webdav-trash-table" ).querySelector( "tbody" );
    await Pager.load( "webdav-trash-table" , "entries" , ( offset ) => Api.listWebDAVTrash( offset ) , function ( entry ) {
      body.appendChild( Dom.el( "tr" , { children: [
        Dom.el( "td" , { class: "text-break" , children: [
          Dom.el( "i" , { class: "bi " + ( entry.is_dir ? "bi-folder" : "bi-file-earmark" ) + " me-2" , attrs: { "aria-hidden": "true" } } ),
          document.createTextNode( entry.path ),
        ] } ),
        Dom.el( "td" , { class: "text-end text-nowrap" , text: WebDAV.size( entry.size ) } ),
        Dom.el( "td" , { class: "d-none d-md-table-cell" , children: [ Dom.when( entry.deleted_at ) ] } ),
        Dom.el( "td" , { children: [ Dom.rowActions( entry.path , [
          Dom.el( "button" , {
            class: "btn btn-sm btn-outline-secondary",
            text: I18n.get( "webdav.restore_button" ),
            on: { click: async function () {
              try {
                await Api.restoreWebDAVFile( entry.id );
                await Promise.all( [ Trash.refresh() , Files.reload() , WebDAV.refreshUsage() ] );
                Dom.toast( I18n.get( "webdav.restored" ) , "ok" );
              } catch ( error ) { App.showError( error.message ); }
            } },
          } ),
          Dom.el( "button" , {
            class: "btn btn-sm btn-outline-danger",
            text: I18n.get( "webdav.empty_button" ),
            on: { click: async function () {
              if ( !( await Dom.confirm( I18n.get( "webdav.empty_confirm" ) ) ) ) return;
              try {
                await Api.emptyWebDAVTrashEntry( entry.id );
                await Promise.all( [ Trash.refresh() , WebDAV.refreshUsage() ] );
              } catch ( error ) { App.showError( error.message ); }
            } },
          } ),
        ] ) ] } ),
      ] } ) );
    } );
  },
};
