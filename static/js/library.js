// The Library screen: the items a client (PRMA, later) pushed beside your
// WebDAV files, searchable, with a link to each attachment's file streamed
// straight out of Zotero's zip. Zotero's own WebDAV sync carries files only,
// so this stays empty until something pushes metadata; the empty row says so.
//
// A plain click on a row opens its editor (Library.edit). Ctrl-click (Cmd on
// a Mac) picks rows, Shift-click picks a range, and the first column's boxes
// do the same on a phone. Delete takes the picked items with their
// attachments and notes, as Zotero does, and offers Undo.
//
// The Open button shows a PDF in a new tab or downloads it, as the account's
// Library setting says (the card on the Account screen, also run from here).
//
// WebDAV.init calls Library.init once it knows WebDAV is on.
const Library = {
  // The picked rows, by item key, and the row a Shift-click counts from.
  selected: new Set(),
  anchor: null,
  // "tab" or "download": me.library_open.
  openMode: "tab",
  // Zotero's item types and creator types, fetched the first time an editor opens.
  schema: null,

  init( me ) {
    this.openMode = me.library_open === "download" ? "download" : "tab";
    Dom.show( Dom.get( "library-panel" ) , true );
    Pager.sortable( "library-table" , () => this.refresh() );
    Pager.search( "library-table" , "library-search" , () => this.refresh() );
    Dom.get( "library-top-only" ).addEventListener( "change" , () => this.refresh() );
    this.wireSelection();
    this.wireEditor();
    this.wireOpenSetting();
    this.refresh().catch( ( error ) => App.showError( error.message ) );
  },

  async refresh() {
    this.selected.clear();
    this.anchor = null;
    this.paintSelection();
    const body = Dom.get( "library-table" ).querySelector( "tbody" );
    const params = Object.assign( {} , Pager.params( "library-table" ) );
    if ( Dom.get( "library-top-only" ).checked ) params.top = "true";
    await Pager.load( "library-table" , "records" , ( offset ) => Api.listLibraryItems( offset , params ) , ( record ) => {
      const name = record.title || record.filename || record.key;
      const title = Dom.el( "div" , { class: "text-break" , text: name } );
      const cell = Dom.el( "td" , { children: [ title ] } );
      if ( record.doi ) {
        cell.appendChild( Dom.el( "div" , { class: "small text-body-secondary text-break" , text: "doi:" + record.doi } ) );
      }
      const box = Dom.el( "input" , { class: "form-check-input" , attrs: {
        type: "checkbox" , "aria-label": I18n.get( "library.select_row" ).replace( "{name}" , () => name ) ,
      } } );
      body.appendChild( Dom.el( "tr" , { attrs: { "data-key": record.key } , children: [
        Dom.el( "td" , { children: [ Dom.el( "div" , { class: "row-actions" , children: [ box ] } ) ] } ),
        cell,
        Dom.el( "td" , { class: "text-break" , text: record.creators || "" } ),
        Dom.el( "td" , { text: record.year || "" } ),
        Dom.el( "td" , { class: "d-none d-lg-table-cell" , text: record.item_type || "" } ),
        Dom.el( "td" , { class: "d-none d-md-table-cell" , text: record.modified || "" } ),
        Dom.el( "td" , { children: [ Library.fileCell( record ) ] } ),
      ] } ) );
    } );
    this.paintSelection();
  },

  fileCell( record ) {
    if ( !record.has_file ) return Dom.el( "span" , { class: "text-body-secondary" , text: I18n.get( "library.no_file" ) } );
    const path = "/api/library/attachments/" + encodeURIComponent( record.file_key || record.key ) + "/file";
    // Only a PDF opens in a tab: the server sends anything else as a download.
    const pdf = record.file_type === "application/pdf" || /\.pdf$/i.test( record.filename || "" );
    const attrs = this.openMode === "tab" && pdf
      ? { href: Api.withLibrary( path + "?inline=true" ) , target: "_blank" , rel: "noopener" }
      : { href: Api.withLibrary( path ) , download: "" };
    return Dom.el( "a" , { class: "btn btn-sm btn-outline-secondary" , text: I18n.get( "library.file_button" ) , attrs: attrs } );
  },

  // ---------------------------------------------------------------------------
  // picking rows
  // ---------------------------------------------------------------------------

  rows() {
    return Array.from( Dom.get( "library-table" ).querySelectorAll( "tbody tr[data-key]" ) );
  },

  wireSelection() {
    const body = Dom.get( "library-table" ).querySelector( "tbody" );
    // Shift-click would also select the text between the rows.
    body.addEventListener( "mousedown" , ( event ) => {
      if ( event.shiftKey && event.target.closest( "tr[data-key]" ) ) event.preventDefault();
    } );
    body.addEventListener( "click" , ( event ) => {
      const row = event.target.closest( "tr[data-key]" );
      if ( !row ) return;
      const key = row.dataset.key;
      const box = event.target.closest( "input[type=checkbox]" );
      if ( box ) {
        if ( event.shiftKey && this.anchor ) this.pickRange( key );
        else this.toggle( key , box.checked );
        return;
      }
      if ( event.target.closest( "a, button, input, select, textarea" ) ) return;
      if ( event.shiftKey && this.anchor ) {
        this.pickRange( key );
      } else if ( event.ctrlKey || event.metaKey ) {
        this.toggle( key , !this.selected.has( key ) );
      } else {
        this.edit( key ).catch( ( error ) => App.showError( error.message ) );
      }
    } );
    Dom.get( "library-select-all" ).addEventListener( "change" , ( event ) => {
      this.rows().forEach( ( row ) => {
        if ( event.target.checked ) this.selected.add( row.dataset.key );
        else this.selected.delete( row.dataset.key );
      } );
      this.anchor = null;
      this.paintSelection();
    } );
    Dom.onPress( Dom.get( "library-clear" ) , () => {
      this.selected.clear();
      this.anchor = null;
      this.paintSelection();
    } );
    Dom.onPress( Dom.get( "library-delete" ) , () => this.deleteSelected() );
  },

  toggle( key , on ) {
    if ( on ) this.selected.add( key );
    else this.selected.delete( key );
    this.anchor = key;
    this.paintSelection();
  },

  // Picks every row from the anchor to key, inclusive, as shown.
  pickRange( key ) {
    const keys = this.rows().map( ( row ) => row.dataset.key );
    const from = keys.indexOf( this.anchor );
    const to = keys.indexOf( key );
    if ( from < 0 || to < 0 ) return this.toggle( key , true );
    keys.slice( Math.min( from , to ) , Math.max( from , to ) + 1 ).forEach( ( each ) => this.selected.add( each ) );
    this.paintSelection();
  },

  paintSelection() {
    const rows = this.rows();
    let shown = 0;
    rows.forEach( ( row ) => {
      const on = this.selected.has( row.dataset.key );
      if ( on ) shown += 1;
      row.classList.toggle( "table-active" , on );
      row.setAttribute( "aria-selected" , on ? "true" : "false" );
      const box = row.querySelector( "input[type=checkbox]" );
      if ( box ) box.checked = on;
    } );
    const all = Dom.get( "library-select-all" );
    all.checked = rows.length > 0 && shown === rows.length;
    all.indeterminate = shown > 0 && shown < rows.length;
    Dom.text( Dom.get( "library-selection-count" ) , I18n.get( "library.selected" ).replace( "{count}" , () => String( this.selected.size ) ) );
    Dom.show( Dom.get( "library-selection" ) , this.selected.size > 0 );
    Dom.show( Dom.get( "library-select-hint" ) , this.selected.size === 0 );
  },

  // Deletes go with Undo rather than a question, as in Files: undo stores
  // every removed record back as it was.
  async deleteSelected() {
    const keys = Array.from( this.selected );
    if ( keys.length === 0 ) return;
    let removed = [];
    try {
      await Undo.run( {
        label: I18n.get( "undo.library_deleted" ),
        toast: true,
        do: async () => {
          removed = await Api.deleteLibraryItems( keys );
          await this.refresh();
        },
        undo: async () => {
          await Api.putLibraryItems( removed );
          await this.refresh();
        },
      } );
    } catch ( error ) { App.showError( error.message ); }
  },

  // ---------------------------------------------------------------------------
  // the Open setting
  // ---------------------------------------------------------------------------

  wireOpenSetting() {
    const select = Dom.get( "library-open-mode" );
    Dom.show( Dom.get( "library-open-panel" ) , true );
    select.value = this.openMode;
    select.addEventListener( "change" , async () => {
      const previous = this.openMode;
      try {
        await Api.setLibraryOpen( select.value );
        this.openMode = select.value;
        Dom.toast( I18n.get( "ui.saved" ) , "ok" );
        await this.refresh();
      } catch ( error ) {
        select.value = previous;
        App.showError( error.message );
      }
    } );
  },

  // ---------------------------------------------------------------------------
  // the editor
  // ---------------------------------------------------------------------------

  // Fields the editor never shows: Zotero's own bookkeeping, and what has its
  // own control (creators, tags). They are kept as they were.
  hidden: new Set( [ "key" , "version" , "itemType" , "parentItem" , "dateAdded" , "dateModified" , "creators" , "tags" ,
    "collections" , "relations" , "deleted" , "inPublications" , "linkMode" , "md5" , "mtime" , "charset" , "path" ] ),
  long: new Set( [ "abstractNote" , "extra" , "note" ] ),

  // What one edit holds while its dialog is open.
  editing: null,

  // "publicationTitle" -> "Publication title"; "DOI" and "url" -> "DOI", "URL".
  fieldLabel( field ) {
    if ( field === "url" ) return "URL";
    if ( field === field.toUpperCase() ) return field;
    const words = field.replace( /([a-z0-9])([A-Z])/g , "$1 $2" ).replace( /([A-Z]+)([A-Z][a-z])/g , "$1 $2" ).split( " " );
    const text = words.map( ( word , index ) => word === word.toUpperCase() && word.length > 1 ? word : ( index === 0 ? word : word.toLowerCase() ) ).join( " " );
    return text.charAt( 0 ).toUpperCase() + text.slice( 1 );
  },

  wireEditor() {
    Dom.get( "library-edit-type" ).addEventListener( "change" , ( event ) => {
      this.editing.type = event.target.value;
      this.drawFields();
      this.drawCreators();
    } );
    Dom.onPress( Dom.get( "library-edit-add-creator" ) , () => {
      const types = this.creatorTypes();
      this.editing.creators.push( { creatorType: types[ 0 ] || "author" , firstName: "" , lastName: "" } );
      this.drawCreators();
      const inputs = Dom.get( "library-edit-creators" ).querySelectorAll( "input" );
      if ( inputs.length ) inputs[ inputs.length - 2 >= 0 ? inputs.length - 2 : 0 ].focus();
    } );
    const element = Dom.get( "library-edit-modal" );
    element.addEventListener( "hide.bs.modal" , ( event ) => {
      if ( this.editing && this.editing.saving ) event.preventDefault();
    } );
    element.addEventListener( "hidden.bs.modal" , () => {
      const back = this.editing && this.editing.returnTo;
      this.editing = null;
      if ( back && back.focus ) back.focus( { preventScroll: true } );
    } );
    Dom.get( "library-edit-form" ).addEventListener( "submit" , ( event ) => {
      event.preventDefault();
      Dom.busy( Dom.get( "library-edit-save" ) , () => this.save() );
    } );
  },

  creatorTypes() {
    return ( this.schema && this.schema.creator_types[ this.editing.type ] ) || [];
  },

  async edit( key ) {
    if ( this.editing ) return;
    const [ record , schema ] = await Promise.all( [ Api.getLibraryItem( key ) , this.schema || Api.librarySchema() ] );
    this.schema = schema;
    const data = record.data || {};
    const values = {};
    Object.entries( data ).forEach( ( [ field , value ] ) => {
      if ( this.hidden.has( field ) ) return;
      if ( typeof value === "string" || typeof value === "number" ) values[ field ] = String( value );
    } );
    this.editing = {
      key: key,
      version: record.version,
      original: data,
      type: data.itemType || record.item_type || "document",
      values: values,
      creators: ( Array.isArray( data.creators ) ? data.creators : [] ).map( ( creator ) => ( { ...creator } ) ),
      returnTo: document.activeElement,
      saving: false,
    };

    const select = Dom.get( "library-edit-type" );
    const types = Object.keys( schema.item_types ).sort();
    if ( !types.includes( this.editing.type ) ) types.unshift( this.editing.type );
    select.replaceChildren( ...types.map( ( type ) => Dom.el( "option" , { text: this.fieldLabel( type ) , attrs: { value: type } } ) ) );
    select.value = this.editing.type;
    Dom.text( Dom.get( "library-edit-key" ) , I18n.get( "library.edit_key" ).replace( "{key}" , () => key ) );
    Dom.get( "library-edit-tags" ).value = ( Array.isArray( data.tags ) ? data.tags : [] ).map( ( tag ) => tag.tag ).filter( Boolean ).join( ", " );
    Dom.show( Dom.get( "library-edit-problem" ) , false );
    this.drawFields();
    this.drawCreators();

    const element = Dom.get( "library-edit-modal" );
    element.addEventListener( "shown.bs.modal" , () => {
      const first = Dom.get( "library-edit-fields" ).querySelector( "input, textarea" );
      if ( first ) first.focus();
    } , { once: true } );
    bootstrap.Modal.getOrCreateInstance( element ).show();
  },

  // The type's own fields in Zotero's order, then any other field the item
  // has, so nothing it holds is out of reach.
  fieldsShown() {
    const own = ( this.schema.item_types[ this.editing.type ] || [] ).filter( ( field ) => !this.hidden.has( field ) );
    const extra = Object.keys( this.editing.values ).filter( ( field ) => !own.includes( field ) && this.editing.values[ field ] !== "" );
    return own.concat( extra );
  },

  drawFields() {
    const editing = this.editing;
    const built = this.fieldsShown().map( ( field , index ) => {
      const id = "library-edit-field-" + index;
      const wide = this.long.has( field ) || field === "title";
      const input = this.long.has( field )
        ? Dom.el( "textarea" , { class: "form-control" , attrs: { id: id , rows: field === "note" ? "10" : "4" } } )
        : Dom.el( "input" , { class: "form-control" , attrs: { id: id } } );
      input.value = editing.values[ field ] || "";
      input.addEventListener( "input" , () => { editing.values[ field ] = input.value; } );
      return Dom.el( "div" , { class: wide ? "col-12" : "col-12 col-md-6" , children: [
        Dom.el( "label" , { class: "form-label" , text: this.fieldLabel( field ) , attrs: { for: id } } ),
        input,
      ] } );
    } );
    Dom.get( "library-edit-fields" ).replaceChildren( ...built );
  },

  drawCreators() {
    const editing = this.editing;
    const types = this.creatorTypes();
    Dom.show( Dom.get( "library-edit-creators-block" ) , types.length > 0 || editing.creators.length > 0 );
    const rows = editing.creators.map( ( creator , index ) => {
      const id = "library-edit-creator-" + index;
      const options = types.includes( creator.creatorType ) || !creator.creatorType ? types : types.concat( [ creator.creatorType ] );
      const select = Dom.el( "select" , { class: "form-select" , attrs: { id: id + "-type" , "aria-label": I18n.get( "library.edit_creator_type" ) } ,
        children: options.map( ( type ) => Dom.el( "option" , { text: this.fieldLabel( type ) , attrs: { value: type } } ) ) } );
      select.value = creator.creatorType || types[ 0 ] || "";
      select.addEventListener( "change" , () => { creator.creatorType = select.value; } );
      const text = ( field , label ) => {
        const input = Dom.el( "input" , { class: "form-control" , attrs: { "aria-label": label , placeholder: label } } );
        input.value = creator[ field ] || "";
        input.addEventListener( "input" , () => { creator[ field ] = input.value; } );
        return input;
      };
      const single = creator.name !== undefined;
      const names = single
        ? [ Dom.el( "div" , { class: "col-12 col-md-6" , children: [ text( "name" , I18n.get( "library.edit_single_name" ) ) ] } ) ]
        : [
          Dom.el( "div" , { class: "col-6 col-md-3" , children: [ text( "lastName" , I18n.get( "library.edit_last_name" ) ) ] } ),
          Dom.el( "div" , { class: "col-6 col-md-3" , children: [ text( "firstName" , I18n.get( "library.edit_first_name" ) ) ] } ),
        ];
      const iconButton = ( icon , label , press , disabled ) => {
        const button = Dom.el( "button" , { class: "btn btn-sm btn-outline-secondary" , attrs: { type: "button" , title: label , "aria-label": label } ,
          children: [ Dom.el( "i" , { class: "bi " + icon , attrs: { "aria-hidden": "true" } } ) ] , on: { click: press } } );
        button.disabled = Boolean( disabled );
        return button;
      };
      const actions = Dom.el( "div" , { class: "col-12 col-md-3 d-flex gap-1 justify-content-md-end" , children: [
        iconButton( single ? "bi-layout-split" : "bi-input-cursor-text" , I18n.get( single ? "library.edit_two_fields" : "library.edit_one_field" ) , () => {
          if ( single ) {
            const parts = ( creator.name || "" ).trim().split( /\s+/ );
            creator.lastName = parts.length > 1 ? parts.pop() : parts[ 0 ] || "";
            creator.firstName = parts.join( " " );
            delete creator.name;
          } else {
            creator.name = [ creator.firstName , creator.lastName ].filter( Boolean ).join( " " );
            delete creator.firstName;
            delete creator.lastName;
          }
          this.drawCreators();
        } ),
        iconButton( "bi-arrow-up" , I18n.get( "library.edit_move_up" ) , () => {
          editing.creators.splice( index - 1 , 0 , editing.creators.splice( index , 1 )[ 0 ] );
          this.drawCreators();
        } , index === 0 ),
        iconButton( "bi-x-lg" , I18n.get( "library.edit_remove" ) , () => {
          editing.creators.splice( index , 1 );
          this.drawCreators();
        } ),
      ] } );
      return Dom.el( "div" , { class: "row g-2 align-items-center" , children: [
        Dom.el( "div" , { class: "col-12 col-md-3" , children: [ select ] } ),
        ...names,
        actions,
      ] } );
    } );
    Dom.get( "library-edit-creators" ).replaceChildren( ...rows );
  },

  // The item's own JSON with the edits on top, so fields this screen does not
  // know about go back as they came.
  edited() {
    const editing = this.editing;
    const data = JSON.parse( JSON.stringify( editing.original ) );
    const before = new Set( this.schema.item_types[ data.itemType ] || [] );
    const now = new Set( this.schema.item_types[ editing.type ] || [] );
    // A field the old type had and the new one does not goes, as in Zotero.
    if ( editing.type !== data.itemType ) before.forEach( ( field ) => { if ( !now.has( field ) ) delete data[ field ]; } );
    data.itemType = editing.type;
    this.fieldsShown().forEach( ( field ) => {
      const value = editing.values[ field ] || "";
      if ( value === "" && !( field in data ) ) return;
      data[ field ] = value;
    } );
    const creators = editing.creators.filter( ( creator ) => ( creator.name || creator.firstName || creator.lastName || "" ).trim() !== "" );
    if ( creators.length || Array.isArray( data.creators ) ) data.creators = creators;
    const oldTags = new Map( ( Array.isArray( data.tags ) ? data.tags : [] ).map( ( tag ) => [ tag.tag , tag ] ) );
    const tags = Dom.get( "library-edit-tags" ).value.split( "," ).map( ( tag ) => tag.trim() ).filter( Boolean )
      .filter( ( tag , index , all ) => all.indexOf( tag ) === index )
      .map( ( tag ) => oldTags.get( tag ) || { tag: tag } );
    if ( tags.length || Array.isArray( data.tags ) ) data.tags = tags;
    data.dateModified = new Date().toISOString().replace( /\.\d{3}Z$/ , "Z" );
    return data;
  },

  async save() {
    const editing = this.editing;
    if ( !editing ) return;
    const problem = Dom.get( "library-edit-problem" );
    Dom.show( problem , false );
    const data = this.edited();
    const previous = editing.original;
    editing.saving = true;
    try {
      await Api.saveLibraryItem( editing.key , editing.version , data );
    } catch ( error ) {
      editing.saving = false;
      Dom.text( problem , Dom.worded( error ) );
      Dom.show( problem , true );
      return;
    }
    editing.saving = false;
    const key = editing.key;
    const version = editing.version;
    Undo.record( {
      label: I18n.get( "undo.library_edited" ),
      undo: async () => { await Api.saveLibraryItem( key , version , previous ); await this.refresh(); },
      redo: async () => { await Api.saveLibraryItem( key , version , data ); await this.refresh(); },
    } );
    bootstrap.Modal.getOrCreateInstance( Dom.get( "library-edit-modal" ) ).hide();
    Dom.toast( I18n.get( "ui.saved" ) , "ok" );
    await this.refresh();
  },
};
