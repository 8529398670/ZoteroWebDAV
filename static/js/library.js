// The Library screen: the items a client (PRMA, later) pushed beside your
// WebDAV files, searchable, with a link to each attachment's file streamed
// straight out of Zotero's zip. Zotero's own WebDAV sync carries files only,
// so this stays empty until something pushes metadata; the empty row says so.
//
// WebDAV.init calls Library.init once it knows WebDAV is on.
const Library = {
  init( me ) {
    Dom.show( Dom.get( "library-panel" ) , true );
    Pager.sortable( "library-table" , () => this.refresh() );
    Pager.search( "library-table" , "library-search" , () => this.refresh() );
    Dom.get( "library-top-only" ).addEventListener( "change" , () => this.refresh() );
    this.refresh().catch( ( error ) => App.showError( error.message ) );
  },

  async refresh() {
    const body = Dom.get( "library-table" ).querySelector( "tbody" );
    const params = Object.assign( {} , Pager.params( "library-table" ) );
    if ( Dom.get( "library-top-only" ).checked ) params.top = "true";
    await Pager.load( "library-table" , "records" , ( offset ) => Api.listLibraryItems( offset , params ) , function ( record ) {
      const title = Dom.el( "div" , { class: "text-break" , text: record.title || record.filename || record.key } );
      const cell = Dom.el( "td" , { children: [ title ] } );
      if ( record.doi ) {
        cell.appendChild( Dom.el( "div" , { class: "small text-body-secondary text-break" , text: "doi:" + record.doi } ) );
      }
      body.appendChild( Dom.el( "tr" , { children: [
        cell,
        Dom.el( "td" , { class: "text-break" , text: record.creators || "" } ),
        Dom.el( "td" , { text: record.year || "" } ),
        Dom.el( "td" , { class: "d-none d-lg-table-cell" , text: record.item_type || "" } ),
        Dom.el( "td" , { class: "d-none d-md-table-cell" , text: record.modified || "" } ),
        Dom.el( "td" , { children: [ Library.fileCell( record ) ] } ),
      ] } ) );
    } );
  },

  fileCell( record ) {
    if ( !record.has_file ) return Dom.el( "span" , { class: "text-body-secondary" , text: I18n.get( "library.no_file" ) } );
    return Dom.el( "a" , {
      class: "btn btn-sm btn-outline-secondary",
      text: I18n.get( "library.file_button" ),
      attrs: { href: Api.withLibrary( "/api/library/attachments/" + encodeURIComponent( record.file_key || record.key ) + "/file" ) , download: "" },
    } );
  },
};
