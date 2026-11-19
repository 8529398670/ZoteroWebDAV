// The Import / Export screen: a Zotero export (or a zip of its folder with
// the files) uploaded in pieces, a Zotero data folder on the server's own
// machine (owner only), and the library downloaded as Zotero RDF, RIS or CSV.
// An import runs on the server; this polls its status while it does.
//
// WebDAV.init calls LibraryIO.init once it knows WebDAV is on.
const LibraryIO = {
  formats: [],
  chunkBytes: 4 * 1024 * 1024,
  // Pieces in flight at once: a slow link stays busy while each waits for
  // its reply. The server writes each at its offset, in any order.
  uploadsAtOnce: 3,
  // Tries per piece; sending a piece again is harmless.
  pieceTries: 3,
  polling: null,

  async init() {
    let reply;
    try {
      reply = await Api.libraryFormats();
    } catch ( error ) {
      if ( error.status === 404 ) return;
      throw error;
    }
    this.formats = reply.formats;
    this.chunkBytes = reply.chunk_bytes;
    Dom.show( Dom.get( "library-io-panel" ) , true );
    Dom.show( Dom.get( "library-io-local-col" ) , reply.local_paths );
    this.fillFormats( Dom.get( "library-io-upload-format" ) , "import" );
    this.fillFormats( Dom.get( "library-io-export-format" ) , "export" );
    Dom.onSubmit( Dom.get( "library-io-upload-form" ) , this.onUpload.bind( this ) );
    Dom.onSubmit( Dom.get( "library-io-local-form" ) , this.onLocal.bind( this ) );
    Dom.get( "library-io-upload-file" ).addEventListener( "change" , () => this.guessFormat() );
    [ "library-io-export-format" , "library-io-export-collection" , "library-io-export-files" ].forEach( ( id ) => {
      Dom.get( id ).addEventListener( "change" , () => this.updateExport() );
    } );
    await this.loadCollections();
    this.updateExport();
    const status = await Api.importStatus();
    if ( status.running || status.phase ) this.show( status );
    if ( status.running ) this.poll();
  },

  // Formats this can do, then the rest, greyed out, as not done yet.
  fillFormats( select , what ) {
    Dom.clear( select );
    const done = this.formats.filter( ( format ) => format[ what ] );
    const todo = this.formats.filter( ( format ) => !format[ what ] );
    done.concat( todo ).forEach( ( format ) => {
      const option = Dom.el( "option" , { text: format[ what ] ? format.label : I18n.get( "library_io.todo" ).replace( "{format}" , () => format.label ) } );
      option.value = format.id;
      option.disabled = !format[ what ];
      select.appendChild( option );
    } );
  },

  async loadCollections() {
    const select = Dom.get( "library-io-export-collection" );
    Dom.clear( select );
    const whole = Dom.el( "option" , { text: I18n.get( "library_io.whole_library" ) } );
    whole.value = "";
    select.appendChild( whole );
    const reply = await Api.listLibraryCollections();
    const records = reply.records || [];
    const children = {};
    records.forEach( ( record ) => {
      const parent = record.parent_key || "";
      ( children[ parent ] = children[ parent ] || [] ).push( record );
    } );
    const add = ( parent , depth ) => {
      ( children[ parent ] || [] ).sort( ( a , b ) => ( a.title || "" ).localeCompare( b.title || "" ) ).forEach( ( record ) => {
        const option = Dom.el( "option" , { text: "  ".repeat( depth ) + ( record.title || record.key ) } );
        option.value = record.key;
        select.appendChild( option );
        if ( depth < 20 ) add( record.key , depth + 1 );
      } );
    };
    add( "" , 0 );
  },

  // The export link and the line under it follow the choices.
  updateExport() {
    const format = Dom.get( "library-io-export-format" ).value;
    const files = Dom.get( "library-io-export-files" ).checked;
    const collection = Dom.get( "library-io-export-collection" ).value;
    const chosen = this.formats.find( ( each ) => each.id === format ) || {};
    Dom.text( Dom.get( "library-io-export-note" ) , I18n.get( chosen.zotero_imports ? "library_io.export_zotero_note" : "library_io.export_csv_note" ) );
    Dom.get( "library-io-export-button" ).setAttribute( "href" , Api.withLibrary( "/api/library/export" + Api.query( {
      format: format , files: files ? "true" : "" , collection: collection ,
    } ) ) );
  },

  // A .ris or .csv file picks its format; anything else leaves the choice.
  guessFormat() {
    const file = Dom.get( "library-io-upload-file" ).files[ 0 ];
    if ( !file ) return;
    const extension = file.name.split( "." ).pop().toLowerCase();
    const found = this.formats.find( ( format ) => format.import && format.ext === extension );
    if ( found ) Dom.get( "library-io-upload-format" ).value = found.id;
  },

  modeOf( name ) {
    const checked = document.querySelector( "input[name=" + name + "]:checked" );
    return checked ? checked.value : "merge";
  },

  async onUpload( event ) {
    event.preventDefault();
    const input = Dom.get( "library-io-upload-file" );
    const file = input.files[ 0 ];
    if ( !file ) {
      Dom.fieldError( input , I18n.get( "library_io.choose_file" ) );
      return;
    }
    const mode = this.modeOf( "library-io-upload-mode" );
    if ( mode === "replace" && !( await Dom.confirm( I18n.get( "library_io.replace_confirm" ) ) ) ) return;
    const format = Dom.get( "library-io-upload-format" ).value;
    try {
      const begun = await Api.importBegin( { format: format , mode: mode , name: file.name , size: file.size } );
      await this.uploadPieces( file , begun.chunk_bytes || this.chunkBytes );
      this.showUploading( file.name , 1 );
      this.show( await Api.importFinish() );
      input.value = "";
      this.poll();
    } catch ( error ) {
      App.showError( error.message );
      Dom.show( Dom.get( "library-io-progress" ) , false );
    }
  },

  // uploadPieces sends the file in pieces of size, a few at once, and
  // throws the first error a piece still has after its tries.
  async uploadPieces( file , size ) {
    let next = 0;
    let sent = 0;
    this.showUploading( file.name , 0 );
    const sendOne = async ( offset ) => {
      const piece = file.slice( offset , Math.min( offset + size , file.size ) );
      for ( let tried = 1; ; tried++ ) {
        try {
          await Api.importChunk( offset , piece );
          break;
        } catch ( error ) {
          // A refusal (4xx) won't change on a second try; a dropped
          // connection or a 5xx might.
          const refused = error.status >= 400 && error.status < 500;
          if ( refused || tried >= this.pieceTries ) throw error;
        }
      }
      sent += piece.size;
      this.showUploading( file.name , sent / file.size );
    };
    let failed = null;
    const worker = async () => {
      while ( failed === null && next < file.size ) {
        const offset = next;
        next += size;
        try {
          await sendOne( offset );
        } catch ( error ) {
          if ( failed === null ) failed = error;
        }
      }
    };
    const workers = [];
    for ( let i = 0; i < this.uploadsAtOnce; i++ ) workers.push( worker() );
    await Promise.all( workers );
    if ( failed !== null ) throw failed;
  },

  async onLocal( event ) {
    event.preventDefault();
    const input = Dom.get( "library-io-local-path" );
    const path = input.value.trim();
    if ( !path ) {
      Dom.fieldError( input , I18n.get( "library_io.choose_path" ) );
      return;
    }
    const mode = this.modeOf( "library-io-local-mode" );
    if ( mode === "replace" && !( await Dom.confirm( I18n.get( "library_io.replace_confirm" ) ) ) ) return;
    try {
      this.show( await Api.importLocal( { path: path , mode: mode , files: Dom.get( "library-io-local-files" ).checked } ) );
      this.poll();
    } catch ( error ) {
      Dom.fieldError( input , error.message );
    }
  },

  poll() {
    window.clearTimeout( this.polling );
    this.polling = window.setTimeout( async () => {
      try {
        const status = await Api.importStatus();
        this.show( status );
        if ( status.running ) {
          this.poll();
        } else if ( status.done ) {
          this.loadCollections().then( () => this.updateExport() );
          if ( typeof Library !== "undefined" && Library.refresh ) Library.refresh().catch( () => {} );
        }
      } catch ( error ) {
        this.poll();
      }
    } , 1000 );
  },

  showUploading( name , fraction ) {
    const percent = Math.floor( fraction * 100 );
    this.show( { running: true , phase: "uploading" , source: name , processed: percent , total: 100 , warnings: [] } );
    Dom.text( Dom.get( "library-io-progress-counts" ) , I18n.get( "library_io.uploading" ).replace( "{percent}" , String( percent ) ) );
  },

  show( status ) {
    Dom.show( Dom.get( "library-io-progress" ) , true );
    const failed = status.phase === "failed";
    const state = failed ? "failed" : status.running ? "running" : "done";
    Dom.text( Dom.get( "library-io-progress-title" ) , I18n.get( "library_io.title_" + state ) +
      ( status.running && status.phase ? " · " + ( I18n.get( "library_io.phase_" + status.phase ) || status.phase ) : "" ) );
    Dom.text( Dom.get( "library-io-progress-source" ) , status.source || "" );
    const badge = Dom.get( "library-io-progress-badge" );
    badge.className = "badge text-bg-" + ( failed ? "danger" : status.running ? "primary" : "success" );
    Dom.text( badge , I18n.get( "library_io.badge_" + state ) );
    const percent = status.total ? Math.min( 100 , Math.round( 100 * status.processed / status.total ) ) : ( status.running ? 0 : 100 );
    const bar = Dom.get( "library-io-bar" );
    bar.style.width = percent + "%";
    bar.className = "progress-bar" + ( status.running ? " progress-bar-striped progress-bar-animated" : failed ? " bg-danger" : " bg-success" );
    Dom.get( "library-io-bar-wrap" ).setAttribute( "aria-valuenow" , String( percent ) );
    let counts = I18n.get( "library_io.counts" )
      .replace( "{collections}" , String( status.collections || 0 ) )
      .replace( "{items}" , String( status.items || 0 ) )
      .replace( "{changed}" , String( status.changed || 0 ) )
      .replace( "{files}" , String( status.files || 0 ) )
      .replace( "{skipped}" , String( status.files_skipped || 0 ) )
      .replace( "{failed}" , String( status.files_failed || 0 ) );
    if ( status.deleted ) counts += " " + I18n.get( "library_io.deleted" ).replace( "{deleted}" , String( status.deleted ) );
    Dom.text( Dom.get( "library-io-progress-counts" ) , counts );
    const error = Dom.get( "library-io-progress-error" );
    Dom.text( error , status.error || "" );
    Dom.show( error , Boolean( status.error ) );
    const warnings = status.warnings || [];
    const list = Dom.get( "library-io-warnings-list" );
    Dom.clear( list );
    warnings.forEach( ( warning ) => list.appendChild( Dom.el( "li" , { text: warning } ) ) );
    Dom.show( Dom.get( "library-io-warnings" ) , warnings.length > 0 );
    Dom.get( "library-io-upload-button" ).disabled = Boolean( status.running );
    Dom.get( "library-io-local-button" ).disabled = Boolean( status.running );
  },
};
