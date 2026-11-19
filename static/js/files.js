// The Files screen: your WebDAV folder as a tree, from the vendored
// Wunderbaum (static/vendor/wunderbaum). Zotero keeps every attachment flat in
// zotero/ as KEY.zip + KEY.prop, so that one folder runs to tens of thousands
// of rows: the tree only draws the rows on screen, and a folder is fetched
// the first time it is opened.
//
// Pick a row to download it (a file) or move it to the trash (either). The
// trash is its own screen (trash.js). WebDAV.render calls Files.init once
// there is a folder to show.
const Files = {
  tree: null,
  // The row picked last, for the Download and Delete buttons.
  picked: null,
  // Rows per request: the most the list route gives at once.
  pageSize: 1000,

  init() {
    if ( this.tree ) return;
    Dom.onPress( Dom.get( "files-delete" ) , () => this.trash( this.picked ) );
    Dom.onPress( Dom.get( "files-reload" ) , () => this.reload() );
    this.tree = new mar10.Wunderbaum( {
      id: "files",
      element: Dom.get( "files-tree" ),
      header: true,
      iconMap: "bootstrap",
      navigationModeOption: "row",
      // Matches --wb-row-outer-height in style.css.
      rowHeightPx: 28,
      columns: this.columns(),
      strings: { noData: I18n.get( "files.empty" ) },
      source: this.fetch( "/" ),
      lazyLoad: ( event ) => this.fetch( event.node.key ),
      render: ( event ) => this.renderRow( event ),
      activate: ( event ) => this.pick( event.node ),
      error: ( event ) => App.showError( String( event.error && event.error.message || event.error ) ),
    } );
    this.wide.addEventListener( "change" , () => {
      this.tree.columns = this.columns();
      this.tree.update( "colStructure" );
    } );
  },

  // Name and size always; on a phone the name needs the room more than when
  // it changed, so Modified only joins from md up, and comes and goes as the
  // window crosses that width.
  wide: window.matchMedia( "(min-width: 768px)" ),

  columns() {
    const columns = [
      { id: "*" , title: I18n.get( "webdav.table_name" ) , width: "*" , minWidth: "120px" },
      { id: "size" , title: I18n.get( "webdav.table_size" ) , width: "90px" , classes: "text-end" },
    ];
    if ( this.wide.matches ) {
      columns.push( { id: "modified" , title: I18n.get( "webdav.table_modified" ) , width: "140px" } );
    }
    return columns;
  },

  // One folder, every page of it, as Wunderbaum rows. A folder is lazy: its
  // own rows are fetched when it is opened.
  async fetch( path ) {
    const rows = [];
    let offset = 0;
    while ( offset !== null && offset !== undefined ) {
      const page = await Api.listWebDAVFiles( path , offset , this.pageSize );
      page.entries.forEach( function ( entry ) {
        const row = {
          title: entry.name,
          key: entry.path,
          lazy: entry.is_dir,
          is_dir: entry.is_dir,
          size: entry.size,
          modified: entry.modified,
        };
        // A folder is left to the tree, which shows it open or shut.
        if ( !entry.is_dir ) row.icon = Files.icon( entry.name );
        rows.push( row );
      } );
      offset = page.next;
    }
    return rows;
  },

  // Zotero's own two kinds get their own icons; anything else is a file.
  icon( name ) {
    if ( /\.zip$/i.test( name ) ) return "bi bi-file-earmark-zip";
    if ( /\.prop$/i.test( name ) ) return "bi bi-file-earmark-code";
    if ( /\.pdf$/i.test( name ) ) return "bi bi-file-earmark-pdf";
    return "bi bi-file-earmark";
  },

  renderRow( event ) {
    const data = event.node.data;
    const cells = event.renderColInfosById;
    if ( cells.size ) cells.size.elem.textContent = data.is_dir ? "" : WebDAV.size( data.size || 0 );
    if ( cells.modified ) {
      const when = new Date( data.modified || "" );
      cells.modified.elem.textContent = isNaN( when.getTime() ) ? "" : Dom.ago( when );
      cells.modified.elem.title = isNaN( when.getTime() ) ? "" : when.toLocaleString();
    }
  },

  pick( node ) {
    this.picked = node;
    Dom.text( Dom.get( "files-selected" ) , node ? node.key : I18n.get( "files.none_selected" ) );
    const download = Dom.get( "files-download" );
    const file = Boolean( node ) && !node.data.is_dir;
    download.classList.toggle( "disabled" , !file );
    download.setAttribute( "aria-disabled" , String( !file ) );
    if ( file ) {
      download.setAttribute( "href" , "/api/webdav/download" + Api.query( { path: node.key } ) );
      download.setAttribute( "download" , node.title );
    } else {
      download.removeAttribute( "href" );
    }
    Dom.get( "files-delete" ).disabled = !node;
  },

  // Delete moves to the trash, so it asks nothing and offers Undo. Deleting
  // zotero/ itself would make Zotero upload everything again, so that asks.
  async trash( node ) {
    if ( !node ) return;
    const path = node.key;
    if ( path === "/zotero" && !( await Dom.confirm( I18n.get( "webdav.zotero_delete_confirm" ) ) ) ) return;
    const parent = node.parent;
    let trashed = null;
    try {
      await Undo.run( {
        label: I18n.get( "undo.webdav_trashed" ),
        toast: true,
        do: async () => {
          trashed = await Api.trashWebDAVFile( path );
          const shown = this.tree.findKey( path );
          if ( shown ) shown.remove();
          this.pick( null );
          await this.afterChange();
        },
        undo: async () => {
          await Api.restoreWebDAVFile( trashed.id );
          await this.reloadFolder( parent );
          await this.afterChange();
        },
      } );
    } catch ( error ) { App.showError( error.message ); }
  },

  async afterChange() {
    await Promise.all( [ Trash.refresh() , WebDAV.refreshUsage() ] );
  },

  // Fetch one folder again, or everything for the top.
  async reloadFolder( node ) {
    if ( !node || node.isRootNode() || !node.lazy ) return this.reload();
    await node.loadLazy( true );
  },

  async reload() {
    if ( !this.tree ) return;
    this.pick( null );
    await this.tree.load( this.fetch( "/" ) );
  },
};
