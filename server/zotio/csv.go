package zotio

import (
	bufio "bufio"
	csv "encoding/csv"
	errors "errors"
	io "io"
	strings "strings"
	unicode "unicode"
)

// CSV, in the columns Zotero's CSV export (CSV.js) writes. Zotero cannot
// import CSV; this reads what Zotero writes, and writes the same for
// spreadsheets.

// csvColumns is Zotero's column list: a field (base names), or
// creators/<type>, tags/own|automatic, attachments/path|url, notes, key,
// publicationYear.
var csvColumns = []string{
	"key" , "itemType" , "publicationYear" , "creators/author" , "title" , "publicationTitle" , "ISBN" , "ISSN" , "DOI" ,
	"url" , "abstractNote" , "date" , "dateAdded" , "dateModified" , "accessDate" , "pages" , "numPages" , "issue" ,
	"volume" , "numberOfVolumes" , "journalAbbreviation" , "shortTitle" , "series" , "seriesNumber" , "seriesText" ,
	"seriesTitle" , "publisher" , "place" , "language" , "rights" , "type" , "archive" , "archiveLocation" ,
	"libraryCatalog" , "callNumber" , "extra" , "notes" , "attachments/path" , "attachments/url" , "tags/own" ,
	"tags/automatic" , "creators/editor" , "creators/seriesEditor" , "creators/translator" , "creators/contributor" ,
	"creators/attorneyAgent" , "creators/bookAuthor" , "creators/castMember" , "creators/commenter" ,
	"creators/composer" , "creators/cosponsor" , "creators/counsel" , "creators/interviewer" , "creators/producer" ,
	"creators/recipient" , "creators/reviewedAuthor" , "creators/scriptwriter" , "creators/wordsBy" , "creators/guest" ,
	"number" , "edition" , "runningTime" , "scale" , "medium" , "artworkSize" , "filingDate" , "applicationNumber" ,
	"assignee" , "issuingAuthority" , "country" , "meetingName" , "conferenceName" , "court" , "references" ,
	"reporter" , "legalStatus" , "priorityNumbers" , "programmingLanguage" , "version" , "system" , "code" ,
	"codeNumber" , "section" , "session" , "committee" , "history" , "legislativeBody" ,
}

// Creator types written in the Author column, as CSV.js does.
var csvAuthorTypes = map[string]bool{
	"author": true , "interviewee": true , "director": true , "artist": true , "sponsor": true , "inventor": true ,
	"cartographer": true , "performer": true , "presenter": true , "podcaster": true , "programmer": true ,
}

// csvLabel is a column's heading: "Item Type", "Manual Tags".
func csvLabel( column string ) ( label string ) {
	parts := strings.Split( column , "/" )
	switch parts[ 0 ] {
	case "creators":
		label = parts[ 1 ]
	case "tags":
		label = "Automatic Tags"
		if parts[ 1 ] == "own" { label = "Manual Tags" }
	case "attachments":
		label = "File Attachments"
		if parts[ 1 ] == "url" { label = "Link Attachments" }
	default:
		label = parts[ 0 ]
	}
	var built strings.Builder
	for index , char := range label {
		if index == 0 {
			built.WriteRune( unicode.ToUpper( char ) )
			continue
		}
		if unicode.IsUpper( char ) && index > 0 && unicode.IsLower( rune( label[ index - 1 ] ) ) { built.WriteRune( ' ' ) }
		built.WriteRune( char )
	}
	label = built.String()
	return
}

// ErrNotCSV is a CSV without Zotero's headings.
var ErrNotCSV = errors.New( "not a Zotero CSV export: no Item Type and Title columns" )

// ReadCSV reads Zotero's CSV export.
func ReadCSV( source io.Reader , bundle Bundle ) ( library *Library , err error ) {
	reader := csv.NewReader( bufio.NewReader( source ) )
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	header , err := reader.Read()
	if err != nil { return }
	by_label := map[string]string{}
	for _ , column := range csvColumns { by_label[ strings.ToLower( csvLabel( column ) ) ] = column }
	columns := make( []string , len( header ) )
	for index , label := range header {
		label = strings.ToLower( strings.TrimSpace( strings.TrimPrefix( label , "\ufeff" ) ) )
		columns[ index ] = by_label[ label ]
	}
	has := func( name string ) ( found bool ) {
		for _ , column := range columns { found = found || column == name }
		return
	}
	if has( "itemType" ) == false || has( "title" ) == false {
		err = ErrNotCSV
		return
	}
	library = NewLibrary()
	keys := newKeyMaker()
	for {
		row , row_err := reader.Read()
		if row_err == io.EOF { break }
		if row_err != nil {
			err = row_err
			return
		}
		values := map[string]string{}
		for index , value := range row {
			if index < len( columns ) && columns[ index ] != "" { values[ columns[ index ] ] = strings.TrimSpace( value ) }
		}
		item_type := values[ "itemType" ]
		if ValidType( item_type ) == false || item_type == "note" || item_type == "attachment" { item_type = "document" }
		item := Item{ "itemType": item_type }
		for _ , column := range csvColumns {
			value := values[ column ]
			if value == "" { continue }
			parts := strings.Split( column , "/" )
			switch parts[ 0 ] {
			case "key" , "publicationYear" , "notes" , "attachments" , "dateAdded" , "dateModified":
			case "creators":
				creator_type := parts[ 1 ]
				if creator_type == "author" && len( creatorTypes[ item_type ] ) > 0 && csvAuthorTypes[ creatorTypes[ item_type ][ 0 ] ] {
					creator_type = creatorTypes[ item_type ][ 0 ]
				}
				for _ , name := range strings.Split( value , ";" ) {
					last , first := splitName( name )
					addCreator( item , creator_type , last , first )
				}
			case "tags":
				for _ , tag := range strings.Split( value , ";" ) { addTag( item , tag , parts[ 1 ] == "automatic" ) }
			case "version":
				setField( item , "versionNumber" , value )
			default:
				setField( item , column , value )
			}
		}
		if values[ "dateAdded" ] != "" { item[ "dateAdded" ] = csvTime( values[ "dateAdded" ] ) }
		if values[ "dateModified" ] != "" { item[ "dateModified" ] = csvTime( values[ "dateModified" ] ) }
		key := values[ "key" ]
		if ValidKey( key ) == false || keys.take( key ) == false { key = keys.make( itemSeed( item ) ) }
		item[ "key" ] = key
		library.Items = append( library.Items , item )
		if note := values[ "notes" ]; note != "" {
			library.Items = append( library.Items , Item{
				"itemType": "note" , "note": note , "parentItem": key , "key": keys.make( key + "|note|" + note ) ,
			} )
		}
		for _ , location := range strings.Split( values[ "attachments/path" ] , ";" ) {
			if location = strings.TrimSpace( location ); location != "" { library.attach( keys , bundle , item , location , "" , "" , "" ) }
		}
		for _ , location := range strings.Split( values[ "attachments/url" ] , ";" ) {
			if location = strings.TrimSpace( location ); location != "" { library.attach( keys , bundle , item , location , "" , "" , "" ) }
		}
	}
	return
}

// csvTime is Zotero's "2020-01-02 03:04:05" as the web API writes it.
func csvTime( value string ) ( result string ) {
	result = value
	if len( value ) == 19 && value[ 10 ] == ' ' { result = value[ :10 ] + "T" + value[ 11: ] + "Z" }
	return
}

// WriteCSV writes library in Zotero's CSV columns. pack, when it carries
// files, names each attachment's file in File Attachments.
func WriteCSV( out io.Writer , library *Library , pack *packer ) ( err error ) {
	buffered := bufio.NewWriterSize( out , 64 * 1024 )
	buffered.WriteString( "\ufeff" )
	writer := csv.NewWriter( buffered )
	header := make( []string , len( csvColumns ) )
	for index , column := range csvColumns { header[ index ] = csvLabel( column ) }
	writer.Write( header )
	built := buildTree( library )
	for _ , item := range built.top {
		item_type := Str( item , "itemType" )
		if item_type == "note" || item_type == "attachment" { continue }
		row := make( []string , len( csvColumns ) )
		for index , column := range csvColumns {
			parts := strings.Split( column , "/" )
			value := ""
			switch parts[ 0 ] {
			case "key":
				value = Str( item , "key" )
			case "publicationYear":
				value = yearOf( Str( item , TypeField( item_type , "date" ) ) )
			case "creators":
				names := []string{}
				for _ , creator := range creatorsOf( item ) {
					creator_type := Str( creator , "creatorType" )
					if creator_type == parts[ 1 ] || ( parts[ 1 ] == "author" && csvAuthorTypes[ creator_type ] ) {
						names = append( names , creatorName( creator ) )
					}
				}
				value = strings.Join( names , "; " )
			case "tags":
				tags , automatic := tagsOf( item )
				wanted := []string{}
				for tag_index , tag := range tags {
					if automatic[ tag_index ] == ( parts[ 1 ] == "automatic" ) { wanted = append( wanted , tag ) }
				}
				value = strings.Join( wanted , "; " )
			case "notes":
				notes := []string{}
				for _ , note := range built.notes( item ) { notes = append( notes , Str( note , "note" ) ) }
				value = strings.Join( notes , "; " )
			case "attachments":
				locations := []string{}
				for _ , attachment := range built.attachments( item ) {
					if parts[ 1 ] == "path" {
						if paths := pack.add( attachment ); len( paths ) > 0 {
							locations = append( locations , paths[ 0 ] )
						} else if Str( attachment , "linkMode" ) == "linked_file" && Str( attachment , "path" ) != "" {
							locations = append( locations , Str( attachment , "path" ) )
						}
					} else if Str( attachment , "linkMode" ) == "linked_url" {
						locations = append( locations , Str( attachment , "url" ) )
					}
				}
				value = strings.Join( locations , "; " )
			case "version":
				value = Str( item , "versionNumber" )
			case "dateAdded" , "dateModified":
				value = strings.TrimSuffix( strings.Replace( Str( item , parts[ 0 ] ) , "T" , " " , 1 ) , "Z" )
			default:
				value = get( item , column )
			}
			row[ index ] = strings.Join( strings.Fields( strings.ReplaceAll( value , "\r" , " " ) ) , " " )
			if parts[ 0 ] == "notes" || parts[ 0 ] == "abstractNote" { row[ index ] = strings.ReplaceAll( strings.ReplaceAll( value , "\r" , "" ) , "\n" , " " ) }
		}
		writer.Write( row )
	}
	writer.Flush()
	if err = writer.Error(); err != nil { return }
	err = buffered.Flush()
	return
}
