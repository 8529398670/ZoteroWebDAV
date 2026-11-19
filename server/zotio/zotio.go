// Package zotio reads and writes the files Zotero exports and imports:
// Zotero RDF, RIS and CSV, and a Zotero data folder (zotero.sqlite and
// storage/) read in place. It knows nothing of HTTP or the database.
//
// Everything goes through one model, Library: items and collections as
// Zotero's own JSON (the "data" object of the Zotero web API, which is what
// the library API stores), and the files that go with each attachment item.
// A reader builds one; a writer turns one back into a file.
//
// Zotero's schema (which fields each item type has, and which of them stand
// for a base field, like bookSection's bookTitle for publicationTitle) is in
// schema_gen.go, generated from a zotero.sqlite.
package zotio

import (
	crypto_rand "crypto/rand"
	sha256 "crypto/sha256"
	errors "errors"
	fmt "fmt"
	io "io"
	sort "sort"
	strings "strings"
	time "time"
)

// Item is one item or collection: Zotero's JSON for it.
type Item = map[string]any

// File is one file of an attachment, opened only when it is copied.
type File struct {
	Name    string
	Size    int64
	ModTime time.Time
	Open    func() ( io.ReadCloser , error )
}

// Library is what a reader found, or what a writer writes.
type Library struct {
	Collections []Item
	Items       []Item
	// Files are each attachment's files, by the attachment item's key.
	Files map[string][]File
	// Lookup, when set, is asked instead of Files: an export reading files
	// only once it reaches them.
	Lookup func( key string ) []File
	// Warnings are things skipped or not found, for the person to read.
	Warnings []string
}

func NewLibrary() ( library *Library ) {
	library = &Library{ Collections: []Item{} , Items: []Item{} , Files: map[string][]File{} }
	return
}

// warn records something skipped. Past a hundred it only counts them.
func ( library *Library ) warn( format string , args ...any ) {
	switch {
	case len( library.Warnings ) < 100:
		library.Warnings = append( library.Warnings , fmt.Sprintf( format , args... ) )
	case len( library.Warnings ) == 100:
		library.Warnings = append( library.Warnings , "more warnings left out" )
	}
}

// ---------------------------------------------------------------------------
// formats
// ---------------------------------------------------------------------------

// Format is one entry in the import/export menus.
type Format struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Ext   string `json:"ext"`
	// Import and Export say what this app does with it; a format with
	// neither is listed so the page can show it as not done yet.
	Import bool `json:"import"`
	Export bool `json:"export"`
	// ZoteroImports says whether Zotero's File > Import reads it back.
	ZoteroImports bool `json:"zotero_imports"`
	// Files says whether its export can carry attachment files.
	Files bool `json:"files"`
}

// Formats is the menu, in the order Zotero lists its own.
//
// TODO: BibLaTeX, BibTeX, CSL JSON, Endnote XML, MODS, Refer/BibIX, TEI,
// Bibliontology RDF, Unqualified Dublin Core RDF, CFF, COinS, Bookmarks,
// Simple Evernote Export, Wikidata QuickStatements, Wikipedia Citation
// Templates. Each needs a reader and/or writer here and its Import/Export
// switched on.
var Formats = []Format{
	{ ID: "rdf" , Label: "Zotero RDF" , Ext: "rdf" , Import: true , Export: true , ZoteroImports: true , Files: true },
	{ ID: "ris" , Label: "RIS" , Ext: "ris" , Import: true , Export: true , ZoteroImports: true , Files: true },
	{ ID: "csv" , Label: "CSV" , Ext: "csv" , Import: true , Export: true , ZoteroImports: false , Files: true },
	{ ID: "biblatex" , Label: "BibLaTeX" , Ext: "bib" },
	{ ID: "bibtex" , Label: "BibTeX" , Ext: "bib" },
	{ ID: "csljson" , Label: "CSL JSON" , Ext: "json" },
	{ ID: "endnote" , Label: "Endnote XML" , Ext: "xml" },
	{ ID: "mods" , Label: "MODS" , Ext: "xml" },
	{ ID: "refer" , Label: "Refer/BibIX" , Ext: "txt" },
	{ ID: "tei" , Label: "TEI" , Ext: "xml" },
	{ ID: "bibliontology" , Label: "Bibliontology RDF" , Ext: "rdf" },
	{ ID: "dc" , Label: "Unqualified Dublin Core RDF" , Ext: "rdf" },
	{ ID: "cff" , Label: "CFF" , Ext: "cff" },
}

// ErrFormat is a format this package cannot do (yet).
var ErrFormat = errors.New( "that format is not supported yet" )

func FindFormat( id string ) ( format Format , found bool ) {
	for _ , each := range Formats {
		if each.ID == id {
			format , found = each , true
			return
		}
	}
	return
}

// ---------------------------------------------------------------------------
// keys
// ---------------------------------------------------------------------------

// keyAlphabet is the characters Zotero makes keys from: no 0, 1, O or I.
const keyAlphabet = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"

// SeedKey is a key made from seed, the same every time: an item read from a
// file with no keys gets the same key when the file is imported again, so it
// updates rather than duplicates.
func SeedKey( seed string ) ( key string ) {
	sum := sha256.Sum256( []byte( seed ) )
	built := make( []byte , 8 )
	for index := range built {
		built[ index ] = keyAlphabet[ int( sum[ index ] ) % len( keyAlphabet ) ]
	}
	key = string( built )
	return
}

// RandomKey is a new key.
func RandomKey() ( key string ) {
	raw := make( []byte , 8 )
	crypto_rand.Read( raw )
	built := make( []byte , 8 )
	for index := range built {
		built[ index ] = keyAlphabet[ int( raw[ index ] ) % len( keyAlphabet ) ]
	}
	key = string( built )
	return
}

// ValidKey is a Zotero key: eight of A-Z and 0-9.
func ValidKey( key string ) ( result bool ) {
	if len( key ) != 8 { return }
	for _ , char := range key {
		if ( char < 'A' || char > 'Z' ) && ( char < '0' || char > '9' ) { return }
	}
	result = true
	return
}

// keyMaker hands out seeded keys, never the same one twice in one import.
type keyMaker struct {
	used map[string]bool
}

func newKeyMaker() ( maker *keyMaker ) {
	maker = &keyMaker{ used: map[string]bool{} }
	return
}

// take records a key the source gave.
func ( maker *keyMaker ) take( key string ) ( fresh bool ) {
	fresh = maker.used[ key ] == false
	maker.used[ key ] = true
	return
}

func ( maker *keyMaker ) make( seed string ) ( key string ) {
	key = SeedKey( seed )
	for attempt := 2; maker.used[ key ]; attempt += 1 {
		key = SeedKey( fmt.Sprintf( "%s#%d" , seed , attempt ) )
	}
	maker.used[ key ] = true
	return
}

// itemSeed is what identifies an item read from a file with no keys.
func itemSeed( item Item ) ( seed string ) {
	first := ""
	if creators , ok := item[ "creators" ].( []any ); ok && len( creators ) > 0 {
		if creator , ok := creators[ 0 ].( map[string]any ); ok {
			first = Str( creator , "lastName" ) + "|" + Str( creator , "name" )
		}
	}
	seed = strings.Join( []string{
		Str( item , "itemType" ) , strings.ToLower( Str( item , "title" ) ) , Str( item , "date" ) ,
		first , strings.ToLower( Str( item , "DOI" ) ) , Str( item , "url" ) ,
	} , "\x1f" )
	return
}

// ---------------------------------------------------------------------------
// fields
// ---------------------------------------------------------------------------

// Str is a field as text, "" when missing or not text.
func Str( item map[string]any , field string ) ( value string ) {
	switch typed := item[ field ].( type ) {
	case string:
		value = typed
	case float64 , int , int64:
		value = fmt.Sprint( typed )
	}
	return
}

// ValidField says whether itemType has field.
func ValidField( item_type string , field string ) ( result bool ) {
	for _ , each := range typeFields[ item_type ] {
		if each == field {
			result = true
			return
		}
	}
	return
}

// ValidType is an item type Zotero knows, notes and attachments included.
func ValidType( item_type string ) ( result bool ) {
	_ , result = typeFields[ item_type ]
	result = result || item_type == "note" || item_type == "attachment"
	return
}

// BaseField is the base field a type's own field stands for, or the field.
func BaseField( item_type string , field string ) ( base string ) {
	base = field
	if mapped , ok := baseFields[ item_type ][ field ]; ok { base = mapped }
	return
}

// TypeField is the field itemType uses for base, "" when it has none.
func TypeField( item_type string , base string ) ( field string ) {
	for own , mapped := range baseFields[ item_type ] {
		if mapped == base {
			field = own
			return
		}
	}
	if ValidField( item_type , base ) { field = base }
	return
}

// setField puts value under the field itemType uses for name (a base field
// or its own), or, when the type has no such field, appends "name: value"
// to extra, as Zotero does with what it cannot place.
func setField( item Item , name string , value string ) {
	value = strings.TrimSpace( value )
	if value == "" { return }
	item_type := Str( item , "itemType" )
	if item_type == "note" || item_type == "attachment" {
		item[ name ] = value
		return
	}
	field := TypeField( item_type , name )
	if field == "" {
		field = TypeField( item_type , BaseField( item_type , name ) )
	}
	if field == "" {
		addExtra( item , name + ": " + value )
		return
	}
	if Str( item , field ) == "" { item[ field ] = value }
}

func addExtra( item Item , line string ) {
	extra := Str( item , "extra" )
	if extra != "" { extra += "\n" }
	item[ "extra" ] = extra + line
}

// get is a base field from an item, through its type's own name for it.
func get( item Item , base string ) ( value string ) {
	field := TypeField( Str( item , "itemType" ) , base )
	if field == "" { field = base }
	value = Str( item , field )
	return
}

// Creator is one creator.
func addCreator( item Item , creator_type string , last string , first string ) {
	last , first = strings.TrimSpace( last ) , strings.TrimSpace( first )
	if last == "" && first == "" { return }
	creator := map[string]any{ "creatorType": creator_type }
	if first == "" {
		creator[ "name" ] = last
	} else {
		creator[ "lastName" ] = last
		creator[ "firstName" ] = first
	}
	creators , _ := item[ "creators" ].( []any )
	item[ "creators" ] = append( creators , creator )
}

// splitName reads "Last, First" or "First Last".
func splitName( text string ) ( last string , first string ) {
	text = strings.TrimSpace( text )
	if comma := strings.Index( text , "," ); comma >= 0 {
		last , first = strings.TrimSpace( text[ :comma ] ) , strings.TrimSpace( text[ comma + 1: ] )
		return
	}
	if space := strings.LastIndex( text , " " ); space >= 0 {
		first , last = strings.TrimSpace( text[ :space ] ) , strings.TrimSpace( text[ space + 1: ] )
		return
	}
	last = text
	return
}

// creatorsOf is an item's creators.
func creatorsOf( item Item ) ( creators []map[string]any ) {
	list , _ := item[ "creators" ].( []any )
	for _ , each := range list {
		if creator , ok := each.( map[string]any ); ok { creators = append( creators , creator ) }
	}
	return
}

// creatorName is "Last, First", or the single name.
func creatorName( creator map[string]any ) ( name string ) {
	name = Str( creator , "name" )
	if name != "" { return }
	name = Str( creator , "lastName" )
	if first := Str( creator , "firstName" ); first != "" { name += ", " + first }
	return
}

func addTag( item Item , tag string , automatic bool ) {
	tag = strings.TrimSpace( tag )
	if tag == "" { return }
	tags , _ := item[ "tags" ].( []any )
	for _ , each := range tags {
		if existing , ok := each.( map[string]any ); ok && Str( existing , "tag" ) == tag { return }
	}
	entry := map[string]any{ "tag": tag }
	if automatic { entry[ "type" ] = 1 }
	item[ "tags" ] = append( tags , entry )
}

// tagsOf is an item's tags, and whether each is automatic.
func tagsOf( item Item ) ( tags []string , automatic []bool ) {
	list , _ := item[ "tags" ].( []any )
	for _ , each := range list {
		entry , ok := each.( map[string]any )
		if ok == false || Str( entry , "tag" ) == "" { continue }
		tags = append( tags , Str( entry , "tag" ) )
		automatic = append( automatic , Str( entry , "type" ) == "1" )
	}
	return
}

func addToCollection( item Item , key string ) {
	list , _ := item[ "collections" ].( []any )
	for _ , each := range list {
		if each == key { return }
	}
	item[ "collections" ] = append( list , key )
}

func collectionsOf( item Item ) ( keys []string ) {
	list , _ := item[ "collections" ].( []any )
	for _ , each := range list {
		if key , ok := each.( string ); ok && key != "" { keys = append( keys , key ) }
	}
	return
}

// ---------------------------------------------------------------------------
// the shape a writer walks
// ---------------------------------------------------------------------------

// tree is a library as writers want it: top-level items in order, each with
// its notes and attachments.
type tree struct {
	top      []Item
	children map[string][]Item
	byKey    map[string]Item
}

func buildTree( library *Library ) ( built *tree ) {
	built = &tree{ children: map[string][]Item{} , byKey: map[string]Item{} }
	for _ , item := range library.Items {
		built.byKey[ Str( item , "key" ) ] = item
	}
	for _ , item := range library.Items {
		parent := Str( item , "parentItem" )
		if parent != "" && built.byKey[ parent ] != nil {
			built.children[ parent ] = append( built.children[ parent ] , item )
			continue
		}
		built.top = append( built.top , item )
	}
	return
}

func ( built *tree ) notes( item Item ) ( notes []Item ) {
	for _ , child := range built.children[ Str( item , "key" ) ] {
		if Str( child , "itemType" ) == "note" { notes = append( notes , child ) }
	}
	return
}

func ( built *tree ) attachments( item Item ) ( attachments []Item ) {
	for _ , child := range built.children[ Str( item , "key" ) ] {
		if Str( child , "itemType" ) == "attachment" { attachments = append( attachments , child ) }
	}
	return
}

// sortedKeys is a map's keys in order, for output that does not change
// between runs.
func sortedKeys[ V any ]( values map[string]V ) ( keys []string ) {
	for key := range values { keys = append( keys , key ) }
	sort.Strings( keys )
	return
}

// stripHTML is a note as plain text, for formats that carry text.
func stripHTML( html string ) ( text string ) {
	var built strings.Builder
	inside := false
	for _ , char := range strings.NewReplacer( "<br>" , "\n" , "<br/>" , "\n" , "<br />" , "\n" , "</p>" , "\n" ).Replace( html ) {
		switch {
		case char == '<':
			inside = true
		case char == '>':
			inside = false
		case inside == false:
			built.WriteRune( char )
		}
	}
	text = strings.TrimSpace( strings.NewReplacer( "&amp;" , "&" , "&lt;" , "<" , "&gt;" , ">" , "&quot;" , `"` , "&#39;" , "'" , "&nbsp;" , " " ).Replace( built.String() ) )
	return
}

// textToHTML is plain text as a note.
func textToHTML( text string ) ( html string ) {
	escaped := strings.NewReplacer( "&" , "&amp;" , "<" , "&lt;" , ">" , "&gt;" ).Replace( strings.TrimSpace( text ) )
	html = "<p>" + strings.ReplaceAll( escaped , "\n" , "<br/>" ) + "</p>"
	return
}

// TypeFields is a copy of every regular item type's fields, in Zotero's
// order, for a screen that edits items. Attachments and notes, which Zotero's
// schema keeps apart, get the fields their JSON carries.
func TypeFields() ( result map[string][]string ) {
	result = make( map[string][]string , len( typeFields ) + 2 )
	for item_type , fields := range typeFields {
		result[ item_type ] = append( []string{} , fields... )
	}
	result[ "attachment" ] = []string{ "title" , "filename" , "contentType" , "url" , "accessDate" }
	result[ "note" ] = []string{ "note" }
	return
}

// CreatorTypes is a copy of each item type's creator types, its main one first.
func CreatorTypes() ( result map[string][]string ) {
	result = make( map[string][]string , len( creatorTypes ) )
	for item_type , types := range creatorTypes {
		result[ item_type ] = append( []string{} , types... )
	}
	return
}
