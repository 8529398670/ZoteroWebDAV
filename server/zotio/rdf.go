package zotio

import (
	bufio "bufio"
	xml "encoding/xml"
	errors "errors"
	fmt "fmt"
	io "io"
	strings "strings"
)

// Zotero RDF: what Zotero's "Zotero RDF" export writes and its RDF import
// reads (the translators "Zotero RDF.js" and "RDF.js"). The writer follows
// Zotero's own export closely; the reader takes what it writes, plus the
// common variants (references to top-level nodes instead of nested ones,
// rdf:Description with rdf:type, the older rdf:resource file path).

const (
	nsRDF     = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
	nsZ       = "http://www.zotero.org/namespaces/export#"
	nsDC      = "http://purl.org/dc/elements/1.1/"
	nsDCTerms = "http://purl.org/dc/terms/"
	nsBib     = "http://purl.org/net/biblio#"
	nsFOAF    = "http://xmlns.com/foaf/0.1/"
	nsLink    = "http://purl.org/rss/1.0/modules/link/"
	nsPrism   = "http://prismstandard.org/namespaces/1.2/basic/"
	nsVCard   = "http://nwalsh.com/rdf/vCard#"
)

// ErrNotRDF is a file that is not RDF/XML.
var ErrNotRDF = errors.New( "not an RDF/XML file" )

// rdfNode is one XML element.
type rdfNode struct {
	name     xml.Name
	attrs    map[xml.Name]string
	children []*rdfNode
	text     strings.Builder
}

func ( node *rdfNode ) is( space string , local string ) ( result bool ) {
	result = node.name.Space == space && node.name.Local == local
	return
}

func ( node *rdfNode ) attr( space string , local string ) ( value string ) {
	value = node.attrs[ xml.Name{ Space: space , Local: local } ]
	return
}

func ( node *rdfNode ) value() ( text string ) {
	text = strings.TrimSpace( node.text.String() )
	return
}

// about is the node's own id, "" for a blank node.
func ( node *rdfNode ) about() ( id string ) {
	id = node.attr( nsRDF , "about" )
	if id == "" {
		if local := node.attr( nsRDF , "ID" ); local != "" { id = "#" + local }
	}
	return
}

// class is what the node is: its element, or its rdf:type for an
// rdf:Description.
func ( node *rdfNode ) class() ( name xml.Name ) {
	name = node.name
	if node.is( nsRDF , "Description" ) {
		for _ , child := range node.children {
			if child.is( nsRDF , "type" ) {
				if resource := child.attr( nsRDF , "resource" ); resource != "" {
					for _ , space := range []string{ nsZ , nsBib , nsFOAF , nsDCTerms , nsVCard } {
						if strings.HasPrefix( resource , space ) {
							name = xml.Name{ Space: space , Local: resource[ len( space ): ] }
							return
						}
					}
				}
			}
		}
	}
	return
}

func parseRDF( reader io.Reader ) ( top []*rdfNode , err error ) {
	decoder := xml.NewDecoder( bufio.NewReader( reader ) )
	decoder.Strict = false
	stack := []*rdfNode{}
	var root *rdfNode
	for {
		token , token_err := decoder.Token()
		if token_err == io.EOF { break }
		if token_err != nil {
			err = fmt.Errorf( "%w: %v" , ErrNotRDF , token_err )
			return
		}
		switch typed := token.( type ) {
		case xml.StartElement:
			node := &rdfNode{ name: typed.Name , attrs: map[xml.Name]string{} }
			for _ , attr := range typed.Attr { node.attrs[ attr.Name ] = attr.Value }
			if len( stack ) > 0 {
				parent := stack[ len( stack ) - 1 ]
				parent.children = append( parent.children , node )
			} else {
				root = node
			}
			stack = append( stack , node )
		case xml.EndElement:
			if len( stack ) > 0 { stack = stack[ :len( stack ) - 1 ] }
		case xml.CharData:
			if len( stack ) > 0 { stack[ len( stack ) - 1 ].text.Write( typed ) }
		}
	}
	if root == nil || root.is( nsRDF , "RDF" ) == false {
		err = ErrNotRDF
		return
	}
	top = root.children
	return
}

// rdfReader walks the parsed nodes.
type rdfReader struct {
	library *Library
	keys    *keyMaker
	bundle  Bundle
	index   map[string]*rdfNode
	items   map[*rdfNode]Item
}

// object is what a property points at: the node nested in it, or the
// top-level node its rdf:resource names. nil when it is a literal.
func ( reader *rdfReader ) object( property *rdfNode ) ( node *rdfNode ) {
	if len( property.children ) > 0 {
		node = property.children[ 0 ]
		if node.about() != "" && len( node.children ) == 0 && reader.index[ node.about() ] != nil {
			node = reader.index[ node.about() ]
		}
		return
	}
	if resource := property.attr( nsRDF , "resource" ); resource != "" { node = reader.index[ resource ] }
	return
}

func ( reader *rdfReader ) literal( property *rdfNode ) ( text string ) {
	if len( property.children ) == 0 {
		text = property.value()
		return
	}
	// <dc:identifier><dcterms:URI><rdf:value>...</rdf:value></dcterms:URI></dc:identifier>
	if node := reader.object( property ); node != nil {
		for _ , child := range node.children {
			if child.is( nsRDF , "value" ) { text = child.value() }
		}
	}
	return
}

var itemClasses = map[string]string{
	"Article": "journalArticle" , "Book": "book" , "BookSection": "bookSection" , "Thesis": "thesis" ,
	"Letter": "letter" , "Manuscript": "manuscript" , "Interview": "interview" , "MotionPicture": "film" ,
	"Illustration": "artwork" , "Document": "document" , "Report": "report" , "Legislation": "statute" ,
	"Patent": "patent" , "Image": "map" , "Recording": "audioRecording" , "ConferenceProceedings": "presentation" ,
	"Data": "computerProgram" ,
}

// ReadRDF reads a Zotero RDF export.
func ReadRDF( source io.Reader , bundle Bundle ) ( library *Library , err error ) {
	top , err := parseRDF( source )
	if err != nil { return }
	library = NewLibrary()
	reader := &rdfReader{ library: library , keys: newKeyMaker() , bundle: bundle , index: map[string]*rdfNode{} , items: map[*rdfNode]Item{} }
	for _ , node := range top {
		if id := node.about(); id != "" { reader.index[ id ] = node }
	}
	// What other nodes point at as children or containers is not an item of
	// its own.
	child_of := map[*rdfNode]*rdfNode{}
	contained := map[*rdfNode]bool{}
	for _ , node := range top {
		for _ , property := range node.children {
			target := reader.object( property )
			if target == nil { continue }
			switch {
			case property.is( nsLink , "link" ) , property.is( nsDCTerms , "isReferencedBy" ):
				child_of[ target ] = node
			case property.is( nsDCTerms , "isPartOf" ) , property.is( nsDC , "publisher" ) , property.is( nsBib , "presentedAt" ):
				contained[ target ] = true
			}
		}
	}
	collections := []*rdfNode{}
	for _ , node := range top {
		class := node.class()
		switch {
		case class.Local == "Collection" && ( class.Space == nsZ || class.Space == nsBib ):
			collections = append( collections , node )
		case child_of[ node ] != nil , contained[ node ]:
		case class.Space == nsZ && class.Local == "Attachment" , class.Space == nsBib && class.Local == "Memo":
			reader.child( node , nil )
		case reader.isItem( node ):
			reader.item( node )
		}
	}
	// Children, once their parents have keys.
	for _ , node := range top {
		if parent := child_of[ node ]; parent != nil {
			if item := reader.items[ parent ]; item != nil {
				reader.child( node , item )
			} else {
				reader.child( node , nil )
			}
		}
	}
	reader.collections( collections )
	return
}

// ownKey is the Zotero key a node's id carries: #item_<KEY> as this app
// writes, or .../items/<KEY> as a Zotero URI. "" when there is none, or it
// was taken already.
func ( reader *rdfReader ) ownKey( node *rdfNode ) ( key string ) {
	id := node.about()
	candidate := ""
	switch {
	case strings.HasPrefix( id , "#item_" ):
		candidate = strings.TrimPrefix( id , "#item_" )
	case strings.Contains( id , "/items/" ):
		candidate = id[ strings.LastIndex( id , "/" ) + 1: ]
	}
	if ValidKey( candidate ) && reader.keys.take( candidate ) { key = candidate }
	return
}

func ( reader *rdfReader ) isItem( node *rdfNode ) ( result bool ) {
	for _ , child := range node.children {
		if child.is( nsZ , "itemType" ) || child.is( nsZ , "type" ) {
			result = true
			return
		}
	}
	class := node.class()
	_ , result = itemClasses[ class.Local ]
	result = result && class.Space == nsBib
	return
}

// itemType is the node's z:itemType, or a guess from its class and
// container.
func ( reader *rdfReader ) itemType( node *rdfNode ) ( item_type string ) {
	for _ , child := range node.children {
		if child.is( nsZ , "itemType" ) || child.is( nsZ , "type" ) {
			if value := child.value(); ValidType( value ) {
				item_type = value
				return
			}
		}
	}
	item_type = itemClasses[ node.class().Local ]
	for _ , child := range node.children {
		if child.is( nsDCTerms , "isPartOf" ) == false { continue }
		container := reader.object( child )
		if container == nil { continue }
		switch container.class().Local {
		case "Periodical":
			item_type = "magazineArticle"
		case "Newspaper":
			item_type = "newspaperArticle"
		case "Website":
			item_type = "webpage"
		case "Blog":
			item_type = "blogPost"
		case "Forum":
			item_type = "forumPost"
		case "CourtReporter":
			item_type = "case"
		}
	}
	if item_type == "" { item_type = "document" }
	return
}

func ( reader *rdfReader ) item( node *rdfNode ) {
	item := Item{ "itemType": reader.itemType( node ) }
	reader.fields( item , node , false )
	key := reader.ownKey( node )
	if key == "" { key = reader.keys.make( itemSeed( item ) ) }
	item[ "key" ] = key
	reader.items[ node ] = item
	reader.library.Items = append( reader.library.Items , item )
}

// fields reads node's properties into item. container is true inside an
// isPartOf, where a title is the publication's.
func ( reader *rdfReader ) fields( item Item , node *rdfNode , container bool ) {
	for _ , property := range node.children {
		space , local := property.name.Space , property.name.Local
		switch {
		case property.is( nsZ , "itemType" ) , property.is( nsZ , "type" ) , property.is( nsZ , "displayTitle" ) ,
			property.is( nsRDF , "type" ) , property.is( nsDC , "relation" ) , property.is( nsZ , "path" ) ,
			property.is( nsLink , "link" ) , property.is( nsDCTerms , "isReferencedBy" ):
		case property.is( nsDC , "title" ):
			if container {
				setField( item , "publicationTitle" , property.value() )
			} else {
				setField( item , "title" , property.value() )
			}
		case property.is( nsDCTerms , "abstract" ):
			setField( item , "abstractNote" , property.value() )
		case property.is( nsDC , "date" ):
			setField( item , "date" , property.value() )
		case property.is( nsDCTerms , "issued" ):
			setField( item , "date" , property.value() )
		case property.is( nsDCTerms , "dateSubmitted" ):
			setField( item , "accessDate" , property.value() )
		case property.is( nsBib , "pages" ):
			setField( item , "pages" , property.value() )
		case property.is( nsDC , "description" ):
			if container {
				setField( item , "seriesText" , property.value() )
			} else {
				setField( item , "extra" , property.value() )
			}
		case property.is( nsDC , "rights" ):
			setField( item , "rights" , property.value() )
		case property.is( nsPrism , "edition" ):
			if ValidField( Str( item , "itemType" ) , "versionNumber" ) {
				setField( item , "versionNumber" , property.value() )
			} else {
				setField( item , "edition" , property.value() )
			}
		case property.is( nsPrism , "volume" ):
			setField( item , "volume" , property.value() )
		case property.is( nsPrism , "number" ):
			if ValidField( Str( item , "itemType" ) , "issue" ) {
				setField( item , "issue" , property.value() )
			} else {
				setField( item , "number" , property.value() )
			}
		case property.is( nsDCTerms , "alternative" ):
			setField( item , "journalAbbreviation" , property.value() )
		case property.is( nsDC , "coverage" ):
			setField( item , "archiveLocation" , property.value() )
		case property.is( nsDCTerms , "medium" ):
			setField( item , "medium" , property.value() )
		case property.is( nsDC , "type" ):
			setField( item , "type" , property.value() )
		case property.is( nsZ , "language" ) , property.is( nsDC , "language" ):
			setField( item , "language" , property.value() )
		case property.is( nsDC , "identifier" ):
			reader.identifier( item , property )
		case property.is( nsDC , "subject" ):
			reader.subject( item , property )
		case property.is( nsDC , "publisher" ):
			if organization := reader.object( property ); organization != nil {
				reader.publisher( item , organization )
			} else {
				setField( item , "publisher" , property.value() )
			}
		case property.is( nsBib , "presentedAt" ):
			if conference := reader.object( property ); conference != nil {
				for _ , child := range conference.children {
					if child.is( nsDC , "title" ) { setField( item , "conferenceName" , child.value() ) }
				}
			}
		case property.is( nsDCTerms , "isPartOf" ):
			reader.partOf( item , property )
		case ( space == nsBib || space == nsZ ) && strings.HasSuffix( local , "s" ) && reader.isCreatorList( property ):
			reader.creators( item , strings.TrimSuffix( local , "s" ) , property )
		case space == nsZ:
			setField( item , local , reader.literal( property ) )
		}
	}
}

func ( reader *rdfReader ) isCreatorList( property *rdfNode ) ( result bool ) {
	node := reader.object( property )
	result = node != nil && ( node.is( nsRDF , "Seq" ) || node.is( nsRDF , "Bag" ) )
	return
}

func ( reader *rdfReader ) creators( item Item , creator_type string , property *rdfNode ) {
	list := reader.object( property )
	for _ , entry := range list.children {
		if entry.is( nsRDF , "li" ) == false { continue }
		person := reader.object( entry )
		if person == nil {
			addCreator( item , creator_type , entry.value() , "" )
			continue
		}
		last , first , name := "" , "" , ""
		for _ , child := range person.children {
			switch {
			case child.is( nsFOAF , "surname" ):
				last = child.value()
			case child.is( nsFOAF , "givenName" ) , child.is( nsFOAF , "givenname" ):
				first = child.value()
			case child.is( nsFOAF , "name" ):
				name = child.value()
			}
		}
		if last == "" { last = name }
		addCreator( item , creator_type , last , first )
	}
}

func ( reader *rdfReader ) identifier( item Item , property *rdfNode ) {
	if node := reader.object( property ); node != nil {
		value := reader.literal( property )
		if node.class().Local == "URI" || strings.HasPrefix( value , "http" ) {
			setField( item , "url" , value )
		}
		return
	}
	value := property.value()
	upper := strings.ToUpper( value )
	switch {
	case strings.HasPrefix( upper , "DOI " ):
		setField( item , "DOI" , value[ 4: ] )
	case strings.HasPrefix( upper , "ISBN " ):
		setField( item , "ISBN" , value[ 5: ] )
	case strings.HasPrefix( upper , "ISSN " ):
		setField( item , "ISSN" , value[ 5: ] )
	case strings.HasPrefix( value , "http://" ) , strings.HasPrefix( value , "https://" ):
		setField( item , "url" , value )
	default:
		setField( item , "accessionNumber" , value )
	}
}

func ( reader *rdfReader ) subject( item Item , property *rdfNode ) {
	node := reader.object( property )
	if node == nil {
		addTag( item , property.value() , false )
		return
	}
	value := reader.literal( property )
	switch node.class().Local {
	case "AutomaticTag":
		addTag( item , value , true )
	case "LCC":
		setField( item , "callNumber" , value )
	default:
		addTag( item , value , false )
	}
}

func ( reader *rdfReader ) publisher( item Item , organization *rdfNode ) {
	for _ , child := range organization.children {
		switch {
		case child.is( nsFOAF , "name" ):
			setField( item , "publisher" , child.value() )
		case child.is( nsVCard , "adr" ):
			if address := reader.object( child ); address != nil {
				for _ , part := range address.children {
					if part.is( nsVCard , "locality" ) { setField( item , "place" , part.value() ) }
				}
			}
		}
	}
}

func ( reader *rdfReader ) partOf( item Item , property *rdfNode ) {
	container := reader.object( property )
	if container == nil { return }
	switch container.class().Local {
	case "Part":
		for _ , child := range container.children {
			if child.is( nsDC , "title" ) { setField( item , "section" , child.value() ) }
		}
		for _ , child := range container.children {
			if child.is( nsDCTerms , "isPartOf" ) { reader.partOf( item , child ) }
		}
	case "Series":
		for _ , child := range container.children {
			switch {
			case child.is( nsDC , "title" ):
				setField( item , "series" , child.value() )
			case child.is( nsDCTerms , "alternative" ):
				setField( item , "seriesTitle" , child.value() )
			case child.is( nsDC , "description" ):
				setField( item , "seriesText" , child.value() )
			case child.is( nsDC , "identifier" ):
				setField( item , "seriesNumber" , child.value() )
			}
		}
	default:
		reader.fields( item , container , true )
	}
}

// child reads an attachment or a note, under parent or on its own.
func ( reader *rdfReader ) child( node *rdfNode , parent Item ) {
	class := node.class()
	parent_key := ""
	if parent != nil { parent_key = Str( parent , "key" ) }
	if class.Local == "Memo" {
		note := Item{ "itemType": "note" }
		for _ , child := range node.children {
			if child.is( nsRDF , "value" ) { note[ "note" ] = child.value() }
			if child.is( nsDC , "subject" ) { reader.subject( note , child ) }
		}
		if Str( note , "note" ) == "" { return }
		note[ "key" ] = reader.ownKey( node )
		if note[ "key" ] == "" { note[ "key" ] = reader.keys.make( parent_key + "|note|" + Str( note , "note" ) ) }
		if parent_key != "" { note[ "parentItem" ] = parent_key }
		reader.library.Items = append( reader.library.Items , note )
		return
	}
	location , title , content_type := "" , "" , ""
	for _ , child := range node.children {
		switch {
		case child.is( nsZ , "path" ) , child.is( nsRDF , "resource" ):
			location = child.attr( nsRDF , "resource" )
			if location == "" { location = child.value() }
		case child.is( nsDC , "title" ):
			title = child.value()
		case child.is( nsLink , "type" ):
			content_type = child.value()
		case child.is( nsDC , "identifier" ) && location == "":
			location = reader.literal( child )
		}
	}
	attachment := reader.library.attach( reader.keys , reader.bundle , parent , location , title , content_type , reader.ownKey( node ) )
	for _ , child := range node.children {
		switch {
		case child.is( nsDC , "identifier" ):
			if value := reader.literal( child ); value != "" && Str( attachment , "url" ) == "" { attachment[ "url" ] = value }
		case child.is( nsDCTerms , "dateSubmitted" ):
			attachment[ "accessDate" ] = child.value()
		case child.is( nsDC , "description" ):
			attachment[ "note" ] = child.value()
		case child.is( nsLink , "charset" ):
			attachment[ "charset" ] = child.value()
		case child.is( nsDC , "subject" ):
			reader.subject( attachment , child )
		}
	}
}

func ( reader *rdfReader ) collections( nodes []*rdfNode ) {
	keys := map[*rdfNode]string{}
	names := map[*rdfNode]string{}
	parent := map[*rdfNode]*rdfNode{}
	for _ , node := range nodes {
		for _ , child := range node.children {
			if child.is( nsDC , "title" ) { names[ node ] = child.value() }
			if child.is( nsDCTerms , "hasPart" ) {
				if target := reader.object( child ); target != nil && target.class().Local == "Collection" { parent[ target ] = node }
			}
		}
	}
	// A key from the collection's path, so the same tree imports the same.
	var path func( node *rdfNode , depth int ) string
	path = func( node *rdfNode , depth int ) ( result string ) {
		result = names[ node ]
		if up := parent[ node ]; up != nil && depth < 50 { result = path( up , depth + 1 ) + "/" + result }
		return
	}
	for _ , node := range nodes { keys[ node ] = reader.keys.make( "collection|" + path( node , 0 ) ) }
	for _ , node := range nodes {
		collection := Item{ "key": keys[ node ] , "name": names[ node ] , "parentCollection": false }
		if up := parent[ node ]; up != nil { collection[ "parentCollection" ] = keys[ up ] }
		reader.library.Collections = append( reader.library.Collections , collection )
		for _ , child := range node.children {
			if child.is( nsDCTerms , "hasPart" ) == false { continue }
			if item := reader.items[ reader.object( child ) ]; item != nil { addToCollection( item , keys[ node ] ) }
		}
	}
}

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

// rdfWriter writes Zotero RDF the way Zotero's own export does.
type rdfWriter struct {
	out  *bufio.Writer
	pack *packer
	ids  map[string]string
}

func esc( value string ) ( result string ) {
	var built strings.Builder
	xml.EscapeText( &built , []byte( value ) )
	result = built.String()
	return
}

func ( writer *rdfWriter ) line( indent int , format string , args ...any ) {
	writer.out.WriteString( strings.Repeat( "    " , indent ) )
	fmt.Fprintf( writer.out , format , args... )
	writer.out.WriteString( "\n" )
}

func ( writer *rdfWriter ) literal( indent int , element string , value string ) {
	if value == "" { return }
	writer.line( indent , "<%s>%s</%s>" , element , esc( value ) , element )
}

var rdfClasses = map[string][2]string{
	"book": { "bib:Book" , "" } , "bookSection": { "bib:BookSection" , "bib:Book" } ,
	"journalArticle": { "bib:Article" , "bib:Journal" } , "magazineArticle": { "bib:Article" , "bib:Periodical" } ,
	"newspaperArticle": { "bib:Article" , "bib:Newspaper" } , "thesis": { "bib:Thesis" , "" } ,
	"letter": { "bib:Letter" , "" } , "manuscript": { "bib:Manuscript" , "" } , "interview": { "bib:Interview" , "" } ,
	"film": { "bib:MotionPicture" , "" } , "artwork": { "bib:Illustration" , "" } , "webpage": { "bib:Document" , "z:Website" } ,
	"report": { "bib:Report" , "" } , "bill": { "bib:Legislation" , "" } , "case": { "bib:Document" , "bib:CourtReporter" } ,
	"hearing": { "bib:Report" , "" } , "patent": { "bib:Patent" , "" } , "statute": { "bib:Legislation" , "" } ,
	"email": { "bib:Letter" , "" } , "map": { "bib:Image" , "" } , "blogPost": { "bib:Document" , "z:Blog" } ,
	"instantMessage": { "bib:Letter" , "" } , "forumPost": { "bib:Document" , "z:Forum" } ,
	"audioRecording": { "bib:Recording" , "" } , "presentation": { "bib:ConferenceProceedings" , "" } ,
	"videoRecording": { "bib:Recording" , "" } , "tvBroadcast": { "bib:Recording" , "" } ,
	"radioBroadcast": { "bib:Recording" , "" } , "podcast": { "bib:Recording" , "" } ,
	"computerProgram": { "bib:Data" , "" } , "encyclopediaArticle": { "rdf:Description" , "bib:Book" } ,
	"dictionaryEntry": { "rdf:Description" , "bib:Book" } , "conferencePaper": { "rdf:Description" , "bib:Journal" } ,
}

// Fields placed by hand; everything else goes out as z:<field>.
var rdfHandled = map[string]bool{
	"title": true , "url": true , "accessionNumber": true , "rights": true , "edition": true , "version": true ,
	"versionNumber": true , "date": true , "accessDate": true , "pages": true , "extra": true , "ISSN": true ,
	"ISBN": true , "DOI": true , "publicationTitle": true , "journalAbbreviation": true , "volume": true ,
	"issue": true , "number": true , "callNumber": true , "abstractNote": true , "series": true ,
	"seriesTitle": true , "seriesText": true , "seriesNumber": true , "publisher": true , "place": true ,
	"archiveLocation": true , "medium": true , "conferenceName": true , "type": true , "section": true ,
	"key": true , "itemType": true , "creators": true , "tags": true , "collections": true ,
	"relations": true , "dateAdded": true , "dateModified": true , "parentItem": true , "note": true ,
	"deleted": true , "inPublications": true ,
}

// WriteRDF writes library as Zotero RDF. pack, when it carries files, puts
// each attachment's files under files/<n>/ and names them in z:path.
func WriteRDF( out io.Writer , library *Library , pack *packer ) ( err error ) {
	writer := &rdfWriter{ out: bufio.NewWriterSize( out , 64 * 1024 ) , pack: pack , ids: map[string]string{} }
	built := buildTree( library )
	// The node id carries the Zotero key, so importing this again here
	// updates the same items. Zotero reads it as an opaque id.
	for index , item := range library.Items {
		id := fmt.Sprintf( "#item_%d" , index + 1 )
		if key := Str( item , "key" ); ValidKey( key ) { id = "#item_" + key }
		writer.ids[ Str( item , "key" ) ] = id
	}
	writer.line( 0 , `<rdf:RDF` )
	writer.line( 1 , `xmlns:rdf="%s"` , nsRDF )
	writer.line( 1 , `xmlns:z="%s"` , nsZ )
	writer.line( 1 , `xmlns:dcterms="%s"` , nsDCTerms )
	writer.line( 1 , `xmlns:bib="%s"` , nsBib )
	writer.line( 1 , `xmlns:foaf="%s"` , nsFOAF )
	writer.line( 1 , `xmlns:link="%s"` , nsLink )
	writer.line( 1 , `xmlns:dc="%s"` , nsDC )
	writer.line( 1 , `xmlns:prism="%s"` , nsPrism )
	writer.line( 1 , `xmlns:vcard="%s">` , nsVCard )
	for _ , item := range built.top {
		writer.item( item , built )
	}
	for _ , collection := range library.Collections { writer.collection( collection , library ) }
	writer.line( 0 , `</rdf:RDF>` )
	err = writer.out.Flush()
	return
}

func ( writer *rdfWriter ) item( item Item , built *tree ) {
	item_type := Str( item , "itemType" )
	id := writer.ids[ Str( item , "key" ) ]
	switch item_type {
	case "note":
		writer.note( item , id )
		return
	case "attachment":
		writer.attachment( item , id )
		return
	}
	classes , found := rdfClasses[ item_type ]
	if found == false { classes = [2]string{ "rdf:Description" , "" } }
	class , container := classes[ 0 ] , classes[ 1 ]
	writer.line( 1 , `<%s rdf:about="%s">` , class , esc( id ) )
	writer.literal( 2 , "z:itemType" , item_type )
	// Fields keyed by their base names.
	base := map[string]string{}
	for field , value := range item {
		if text , ok := value.( string ); ok && text != "" { base[ BaseField( item_type , field ) ] = text }
	}
	series := base[ "series" ] != "" || base[ "seriesTitle" ] != "" || base[ "seriesText" ] != "" || base[ "seriesNumber" ] != ""
	writeSeries := func( indent int ) {
		if series == false { return }
		writer.line( indent , `<dcterms:isPartOf>` )
		writer.line( indent + 1 , `<bib:Series>` )
		writer.literal( indent + 2 , "dc:title" , base[ "series" ] )
		writer.literal( indent + 2 , "dcterms:alternative" , base[ "seriesTitle" ] )
		writer.literal( indent + 2 , "dc:description" , base[ "seriesText" ] )
		writer.literal( indent + 2 , "dc:identifier" , base[ "seriesNumber" ] )
		writer.line( indent + 1 , `</bib:Series>` )
		writer.line( indent , `</dcterms:isPartOf>` )
	}
	// Fields that belong on the container when there is one.
	containerFields := func( indent int ) {
		writer.literal( indent , "dc:title" , base[ "publicationTitle" ] )
		writer.literal( indent , "dcterms:alternative" , base[ "journalAbbreviation" ] )
		writer.literal( indent , "prism:volume" , base[ "volume" ] )
		number := base[ "issue" ]
		if number == "" { number = base[ "number" ] }
		writer.literal( indent , "prism:number" , number )
		for _ , pair := range [][2]string{ { "ISSN" , "ISSN " } , { "ISBN" , "ISBN " } , { "DOI" , "DOI " } } {
			if base[ pair[ 0 ] ] != "" { writer.literal( indent , "dc:identifier" , pair[ 1 ] + base[ pair[ 0 ] ] ) }
		}
	}
	if container != "" {
		writer.line( 2 , `<dcterms:isPartOf>` )
		writer.line( 3 , `<%s>` , container )
		writeSeries( 4 )
		containerFields( 4 )
		writer.line( 3 , `</%s>` , container )
		writer.line( 2 , `</dcterms:isPartOf>` )
	} else {
		writeSeries( 2 )
	}
	if base[ "section" ] != "" {
		writer.line( 2 , `<dcterms:isPartOf><bib:Part><dc:title>%s</dc:title></bib:Part></dcterms:isPartOf>` , esc( base[ "section" ] ) )
	}
	if base[ "publisher" ] != "" || base[ "place" ] != "" {
		writer.line( 2 , `<dc:publisher>` )
		writer.line( 3 , `<foaf:Organization>` )
		if base[ "place" ] != "" {
			writer.line( 4 , `<vcard:adr><vcard:Address><vcard:locality>%s</vcard:locality></vcard:Address></vcard:adr>` , esc( base[ "place" ] ) )
		}
		writer.literal( 4 , "foaf:name" , base[ "publisher" ] )
		writer.line( 3 , `</foaf:Organization>` )
		writer.line( 2 , `</dc:publisher>` )
	}
	writer.creators( item )
	for _ , note := range built.notes( item ) {
		writer.line( 2 , `<dcterms:isReferencedBy rdf:resource="%s"/>` , esc( writer.ids[ Str( note , "key" ) ] ) )
	}
	for _ , attachment := range built.attachments( item ) {
		writer.line( 2 , `<link:link rdf:resource="%s"/>` , esc( writer.ids[ Str( attachment , "key" ) ] ) )
	}
	writer.tags( item , 2 )
	writer.literal( 2 , "dc:title" , base[ "title" ] )
	writer.literal( 2 , "dcterms:abstract" , base[ "abstractNote" ] )
	writer.literal( 2 , "dc:date" , base[ "date" ] )
	writer.literal( 2 , "bib:pages" , base[ "pages" ] )
	if base[ "url" ] != "" {
		writer.line( 2 , `<dc:identifier><dcterms:URI><rdf:value>%s</rdf:value></dcterms:URI></dc:identifier>` , esc( base[ "url" ] ) )
	}
	writer.literal( 2 , "dc:identifier" , base[ "accessionNumber" ] )
	writer.literal( 2 , "dcterms:dateSubmitted" , base[ "accessDate" ] )
	writer.literal( 2 , "dc:rights" , base[ "rights" ] )
	edition := base[ "edition" ]
	if edition == "" { edition = Str( item , "versionNumber" ) }
	writer.literal( 2 , "prism:edition" , edition )
	writer.literal( 2 , "dc:description" , base[ "extra" ] )
	writer.literal( 2 , "dc:coverage" , base[ "archiveLocation" ] )
	writer.literal( 2 , "dcterms:medium" , base[ "medium" ] )
	writer.literal( 2 , "dc:type" , base[ "type" ] )
	if base[ "conferenceName" ] != "" {
		writer.line( 2 , `<bib:presentedAt><bib:Conference><dc:title>%s</dc:title></bib:Conference></bib:presentedAt>` , esc( base[ "conferenceName" ] ) )
	}
	if base[ "callNumber" ] != "" {
		writer.line( 2 , `<dc:subject><dcterms:LCC><rdf:value>%s</rdf:value></dcterms:LCC></dc:subject>` , esc( base[ "callNumber" ] ) )
	}
	if container == "" { containerFields( 2 ) }
	for _ , field := range sortedKeys( item ) {
		if rdfHandled[ field ] || rdfHandled[ BaseField( item_type , field ) ] { continue }
		writer.literal( 2 , "z:" + field , Str( item , field ) )
	}
	writer.line( 1 , `</%s>` , class )
	for _ , note := range built.notes( item ) { writer.note( note , writer.ids[ Str( note , "key" ) ] ) }
	for _ , attachment := range built.attachments( item ) { writer.attachment( attachment , writer.ids[ Str( attachment , "key" ) ] ) }
}

func ( writer *rdfWriter ) creators( item Item ) {
	groups := map[string][]map[string]any{}
	order := []string{}
	for _ , creator := range creatorsOf( item ) {
		creator_type := Str( creator , "creatorType" )
		if creator_type == "" { creator_type = "author" }
		element := "z:" + creator_type + "s"
		if creator_type == "author" || creator_type == "editor" || creator_type == "contributor" { element = "bib:" + creator_type + "s" }
		if groups[ element ] == nil { order = append( order , element ) }
		groups[ element ] = append( groups[ element ] , creator )
	}
	for _ , element := range order {
		writer.line( 2 , `<%s>` , element )
		writer.line( 3 , `<rdf:Seq>` )
		for _ , creator := range groups[ element ] {
			writer.line( 4 , `<rdf:li>` )
			writer.line( 5 , `<foaf:Person>` )
			last := Str( creator , "lastName" )
			if last == "" { last = Str( creator , "name" ) }
			writer.literal( 6 , "foaf:surname" , last )
			writer.literal( 6 , "foaf:givenName" , Str( creator , "firstName" ) )
			writer.line( 5 , `</foaf:Person>` )
			writer.line( 4 , `</rdf:li>` )
		}
		writer.line( 3 , `</rdf:Seq>` )
		writer.line( 2 , `</%s>` , element )
	}
}

func ( writer *rdfWriter ) tags( item Item , indent int ) {
	tags , automatic := tagsOf( item )
	for index , tag := range tags {
		if automatic[ index ] {
			writer.line( indent , `<dc:subject><z:AutomaticTag><rdf:value>%s</rdf:value></z:AutomaticTag></dc:subject>` , esc( tag ) )
			continue
		}
		writer.literal( indent , "dc:subject" , tag )
	}
}

func ( writer *rdfWriter ) note( item Item , id string ) {
	writer.line( 1 , `<bib:Memo rdf:about="%s">` , esc( id ) )
	writer.tags( item , 2 )
	writer.literal( 2 , "rdf:value" , Str( item , "note" ) )
	writer.line( 1 , `</bib:Memo>` )
}

var rdfLinkModes = map[string]string{ "imported_file": "0" , "imported_url": "1" , "linked_file": "2" , "linked_url": "3" }

func ( writer *rdfWriter ) attachment( item Item , id string ) {
	writer.line( 1 , `<z:Attachment rdf:about="%s">` , esc( id ) )
	writer.literal( 2 , "z:itemType" , "attachment" )
	if paths := writer.pack.add( item ); len( paths ) > 0 {
		writer.line( 2 , `<z:path rdf:resource="%s"/>` , esc( paths[ 0 ] ) )
	} else if Str( item , "linkMode" ) == "linked_file" && Str( item , "path" ) != "" {
		writer.line( 2 , `<z:path rdf:resource="%s"/>` , esc( Str( item , "path" ) ) )
	}
	writer.tags( item , 2 )
	if value := Str( item , "url" ); value != "" {
		writer.line( 2 , `<dc:identifier><dcterms:URI><rdf:value>%s</rdf:value></dcterms:URI></dc:identifier>` , esc( value ) )
	}
	writer.literal( 2 , "dcterms:dateSubmitted" , Str( item , "accessDate" ) )
	writer.literal( 2 , "dc:title" , Str( item , "title" ) )
	writer.literal( 2 , "dc:description" , Str( item , "note" ) )
	writer.literal( 2 , "z:linkMode" , rdfLinkModes[ Str( item , "linkMode" ) ] )
	writer.literal( 2 , "link:type" , Str( item , "contentType" ) )
	writer.literal( 2 , "link:charset" , Str( item , "charset" ) )
	writer.line( 1 , `</z:Attachment>` )
}

func ( writer *rdfWriter ) collection( collection Item , library *Library ) {
	key := Str( collection , "key" )
	writer.line( 1 , `<z:Collection rdf:about="#collection_%s">` , esc( key ) )
	writer.literal( 2 , "dc:title" , Str( collection , "name" ) )
	for _ , child := range library.Collections {
		if Str( child , "parentCollection" ) == key {
			writer.line( 2 , `<dcterms:hasPart rdf:resource="#collection_%s"/>` , esc( Str( child , "key" ) ) )
		}
	}
	for _ , item := range library.Items {
		for _ , member := range collectionsOf( item ) {
			if member == key {
				writer.line( 2 , `<dcterms:hasPart rdf:resource="%s"/>` , esc( writer.ids[ Str( item , "key" ) ] ) )
			}
		}
	}
	writer.line( 1 , `</z:Collection>` )
}
