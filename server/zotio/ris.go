package zotio

import (
	bufio "bufio"
	fmt "fmt"
	io "io"
	regexp "regexp"
	strings "strings"
)

// RIS, mapped the way Zotero's RIS translator (RIS.js) maps it, so what this
// writes Zotero reads back into the same fields, and the other way round.

var risExportTypes = map[string]string{
	"artwork": "ART" , "audioRecording": "SOUND" , "bill": "BILL" , "blogPost": "BLOG" , "book": "BOOK" ,
	"bookSection": "CHAP" , "case": "CASE" , "computerProgram": "COMP" , "conferencePaper": "CONF" ,
	"dictionaryEntry": "DICT" , "encyclopediaArticle": "ENCYC" , "email": "ICOMM" , "dataset": "DATA" ,
	"film": "MPCT" , "hearing": "HEAR" , "journalArticle": "JOUR" , "letter": "PCOMM" , "magazineArticle": "MGZN" ,
	"manuscript": "MANSCPT" , "map": "MAP" , "newspaperArticle": "NEWS" , "patent": "PAT" , "presentation": "SLIDE" ,
	"report": "RPRT" , "statute": "STAT" , "thesis": "THES" , "videoRecording": "VIDEO" , "webpage": "ELEC" ,
	// Degenerate: these come back as another type.
	"interview": "PCOMM" , "instantMessage": "ICOMM" , "forumPost": "ICOMM" , "tvBroadcast": "MPCT" ,
	"radioBroadcast": "SOUND" , "podcast": "SOUND" , "document": "GEN" ,
}

var risImportTypes = map[string]string{
	"ABST": "journalArticle" , "ADVS": "film" , "AGGR": "document" , "ANCIENT": "document" , "CHART": "artwork" ,
	"CLSWK": "book" , "CPAPER": "conferencePaper" , "CTLG": "magazineArticle" , "DBASE": "dataset" , "EBOOK": "book" ,
	"ECHAP": "bookSection" , "EDBOOK": "book" , "EJOUR": "journalArticle" , "EQUA": "document" , "FIGURE": "artwork" ,
	"GEN": "journalArticle" , "GOVDOC": "report" , "GRNT": "document" , "INPR": "manuscript" , "JFULL": "journalArticle" ,
	"LEGAL": "case" , "MULTI": "videoRecording" , "MUSIC": "audioRecording" , "PAMP": "manuscript" , "SER": "book" ,
	"STAND": "report" , "UNBILL": "manuscript" , "UNPD": "manuscript" , "WEB": "webpage" ,
	"ART": "artwork" , "SOUND": "audioRecording" , "BILL": "bill" , "BLOG": "blogPost" , "BOOK": "book" ,
	"CHAP": "bookSection" , "CASE": "case" , "COMP": "computerProgram" , "CONF": "conferencePaper" ,
	"DICT": "dictionaryEntry" , "ENCYC": "encyclopediaArticle" , "ICOMM": "email" , "DATA": "dataset" ,
	"MPCT": "film" , "HEAR": "hearing" , "JOUR": "journalArticle" , "PCOMM": "letter" , "MGZN": "magazineArticle" ,
	"MANSCPT": "manuscript" , "MAP": "map" , "NEWS": "newspaperArticle" , "PAT": "patent" , "SLIDE": "presentation" ,
	"RPRT": "report" , "STAT": "statute" , "THES": "thesis" , "VIDEO": "videoRecording" , "ELEC": "webpage" ,
}

// risRule is where one tag goes: def for any type not excluded, or the
// target its type names.
type risRule struct {
	def     string
	by      map[string]string
	exclude []string
}

func ( rule risRule ) target( item_type string ) ( target string ) {
	if found , ok := rule.by[ item_type ]; ok {
		target = found
		return
	}
	for _ , excluded := range rule.exclude {
		if excluded == item_type { return }
	}
	target = rule.def
	return
}

// by turns "field: type type ..." pairs into a map.
func by( pairs ...string ) ( result map[string]string ) {
	result = map[string]string{}
	for index := 0; index + 1 < len( pairs ); index += 2 {
		for _ , item_type := range strings.Fields( pairs[ index + 1 ] ) { result[ item_type ] = pairs[ index ] }
	}
	return
}

var authorRule = risRule{ def: "creators/author" , by: by(
	"creators/artist" , "artwork" , "creators/cartographer" , "map" , "creators/composer" , "audioRecording" ,
	"creators/director" , "film radioBroadcast tvBroadcast videoRecording" , "creators/interviewee" , "interview" ,
	"creators/inventor" , "patent" , "creators/podcaster" , "podcast" , "creators/programmer" , "computerProgram" ) }

var dateRule = risRule{ def: "date" , by: by( "dateEnacted" , "statute" , "dateDecided" , "case" , "issueDate" , "patent" ) }

var titleRule = risRule{ def: "title" , by: by( "subject" , "email" , "caseName" , "case" , "nameOfAct" , "statute" ) }

// risExportOrder is the tags written, in order, after TY.
var risExportOrder = []string{ "TI" , "T2" , "T3" , "AU" , "A2" , "A3" , "A4" , "C1" , "C2" , "C3" , "C4" , "C5" , "C6" ,
	"AB" , "DA" , "PY" , "VL" , "IS" , "SP" , "SE" , "SV" , "NV" , "ET" , "M1" , "M3" , "CY" , "PB" , "SN" , "OP" ,
	"LA" , "ST" , "J2" , "DO" , "UR" , "Y2" , "DB" , "DP" , "AN" , "CN" }

var risFields = map[string]risRule{
	"AB": { def: "abstractNote" } , "AN": { def: "archiveLocation" } , "CN": { def: "callNumber" } ,
	"DB": { def: "archive" } , "DO": { def: "DOI" } , "DP": { def: "libraryCatalog" } , "J2": { def: "journalAbbreviation" } ,
	"KW": { def: "tags" } , "L1": { def: "attachments/PDF" } , "L2": { def: "attachments/HTML" } ,
	"L4": { def: "attachments/other" } , "N1": { def: "notes" } , "ST": { def: "shortTitle" } , "UR": { def: "url" } ,
	"Y2": { def: "accessDate" } ,
	"TI": titleRule ,
	"T2": { by: by( "code" , "bill statute" , "bookTitle" , "bookSection" , "blogTitle" , "blogPost" ,
		"conferenceName" , "conferencePaper" , "dictionaryTitle" , "dictionaryEntry" , "encyclopediaTitle" , "encyclopediaArticle" ,
		"committee" , "hearing" , "forumTitle" , "forumPost" , "websiteTitle" , "webpage" , "programTitle" , "radioBroadcast tvBroadcast" ,
		"meetingName" , "presentation" , "seriesTitle" , "computerProgram map report" , "series" , "book" ,
		"publicationTitle" , "journalArticle magazineArticle newspaperArticle" ) } ,
	"T3": { by: by( "legislativeBody" , "hearing bill" , "series" , "bookSection conferencePaper journalArticle" , "seriesTitle" , "audioRecording" ) } ,
	"AU": authorRule , "A1": authorRule ,
	"A2": { by: by( "creators/sponsor" , "bill" , "creators/performer" , "audioRecording" , "creators/presenter" , "presentation" ,
		"creators/interviewer" , "interview" , "creators/editor" , "journalArticle bookSection conferencePaper dictionaryEntry document encyclopediaArticle" ,
		"creators/seriesEditor" , "book report" , "creators/recipient" , "email instantMessage letter" , "reporter" , "case" ,
		"issuingAuthority" , "patent" ) } ,
	"A3": { by: by( "creators/contributor" , "thesis" , "creators/cosponsor" , "bill" , "creators/producer" , "film tvBroadcast videoRecording radioBroadcast" ,
		"creators/editor" , "book" , "creators/seriesEditor" , "bookSection conferencePaper dictionaryEntry encyclopediaArticle map" ) } ,
	"A4": { def: "creators/translator" , by: by( "creators/counsel" , "case" , "creators/contributor" , "conferencePaper film dataset" ) } ,
	"C1": { by: by( "filingDate" , "patent" , "creators/castMember" , "radioBroadcast tvBroadcast videoRecording" , "scale" , "map" , "place" , "conferencePaper" ) } ,
	"C2": { by: by( "issueDate" , "patent" , "creators/bookAuthor" , "bookSection" , "creators/commenter" , "blogPost" ) } ,
	"C3": { by: by( "artworkSize" , "artwork" , "proceedingsTitle" , "conferencePaper" , "country" , "patent" ) } ,
	"C4": { by: by( "creators/wordsBy" , "audioRecording" , "creators/attorneyAgent" , "patent" , "genre" , "film" ) } ,
	"C5": { by: by( "references" , "patent" , "audioRecordingFormat" , "audioRecording radioBroadcast" , "videoRecordingFormat" , "film tvBroadcast videoRecording" ) } ,
	"C6": { by: by( "legalStatus" , "patent" ) } ,
	"CY": { def: "place" , by: by( "repositoryLocation" , "dataset" ) , exclude: []string{ "conferencePaper" } } ,
	"DA": dateRule , "PY": dateRule ,
	"ET": { def: "edition" , by: by( "session" , "bill hearing statute" , "versionNumber" , "computerProgram dataset" ) } ,
	"IS": { def: "issue" , by: by( "numberOfVolumes" , "bookSection" ) , exclude: []string{ "dataset" } } ,
	"LA": { def: "language" , by: by( "programmingLanguage" , "computerProgram" ) } ,
	"M1": { by: by( "seriesNumber" , "book" , "billNumber" , "bill" , "system" , "computerProgram" , "documentNumber" , "hearing" ,
		"applicationNumber" , "patent" , "publicLawNumber" , "statute" , "episodeNumber" , "podcast radioBroadcast tvBroadcast" ) } ,
	"M3": { by: by( "manuscriptType" , "manuscript" , "mapType" , "map" , "reportType" , "report" , "thesisType" , "thesis" ,
		"websiteType" , "blogPost webpage" , "postType" , "forumPost" , "letterType" , "letter" , "interviewMedium" , "interview" ,
		"presentationType" , "presentation" , "artworkMedium" , "artwork" , "audioFileType" , "podcast" ) } ,
	"NV": { def: "numberOfVolumes" , by: by( "identifier" , "dataset" ) , exclude: []string{ "bookSection" } } ,
	"OP": { by: by( "history" , "hearing statute bill case" , "priorityNumbers" , "patent" ) } ,
	"PB": { def: "publisher" , by: by( "label" , "audioRecording" , "court" , "case" , "distributor" , "film" , "assignee" , "patent" ,
		"institution" , "report" , "repository" , "dataset" , "university" , "thesis" , "company" , "computerProgram" ,
		"studio" , "videoRecording" , "network" , "radioBroadcast tvBroadcast" ) } ,
	"SE": { def: "section" , exclude: []string{ "case" } } ,
	"SN": { def: "ISBN" , by: by( "ISSN" , "journalArticle magazineArticle newspaperArticle" , "patentNumber" , "patent" , "reportNumber" , "report" ) } ,
	"SP": { def: "pages" , by: by( "codePages" , "bill" , "numPages" , "book thesis manuscript" , "firstPage" , "case" , "runningTime" , "film" ) } ,
	"SV": { by: by( "seriesNumber" , "bookSection" , "docketNumber" , "case" ) } ,
	"VL": { def: "volume" , by: by( "codeNumber" , "statute" , "codeVolume" , "bill" , "reporterVolume" , "case" ) , exclude: []string{ "patent" , "webpage" } } ,
}

// Read only, for what other programs write.
var risImportOnly = map[string]risRule{
	"T1": titleRule , "Y1": dateRule , "CT": { def: "title" } , "CR": { def: "rights" } , "ED": { def: "creators/editor" } ,
	"EP": { def: "endPage" } , "JA": { def: "journalAbbreviation" } , "JF": { def: "publicationTitle" } ,
	"JO": { def: "journalAbbreviation" , by: by( "conferenceName" , "conferencePaper" ) } , "M2": { def: "extra" } ,
	"N2": { def: "abstractNote" } , "RN": { def: "notes" } , "AV": { def: "archiveLocation" } ,
	"BT": { def: "publicationTitle" , by: by( "title" , "book manuscript" , "bookTitle" , "bookSection" ) } ,
	"ID": { def: "ignore" } ,
}

var risLine = regexp.MustCompile( `^([A-Z][A-Z0-9])  ?-(?: (.*))?$` )

// ReadRIS reads a RIS file.
func ReadRIS( source io.Reader , bundle Bundle ) ( library *Library , err error ) {
	library = NewLibrary()
	keys := newKeyMaker()
	scanner := bufio.NewScanner( source )
	scanner.Buffer( make( []byte , 0 , 64 * 1024 ) , 16 * 1024 * 1024 )
	record := []risEntry{}
	flush := func() {
		if len( record ) > 0 { risRecord( library , keys , bundle , record ) }
		record = record[ :0 ]
	}
	first := true
	for scanner.Scan() {
		text := scanner.Text()
		if first {
			text = strings.TrimPrefix( text , "\ufeff" )
			first = false
		}
		text = strings.TrimRight( text , "\r" )
		match := risLine.FindStringSubmatch( text )
		switch {
		case match != nil && match[ 1 ] == "ER":
			flush()
		case match != nil:
			record = append( record , risEntry{ match[ 1 ] , strings.TrimSpace( match[ 2 ] ) } )
		case strings.TrimSpace( text ) != "" && len( record ) > 0:
			// A value that runs on over lines.
			record[ len( record ) - 1 ].value += "\n" + strings.TrimSpace( text )
		}
	}
	flush()
	err = scanner.Err()
	return
}

type risEntry struct{ tag , value string }

func risRecord( library *Library , keys *keyMaker , bundle Bundle , record []risEntry ) {
	item_type := "journalArticle"
	for _ , pair := range record {
		if pair.tag == "TY" {
			if found , ok := risImportTypes[ strings.ToUpper( pair.value ) ]; ok { item_type = found }
		}
	}
	item := Item{ "itemType": item_type }
	notes := []string{}
	type file struct{ location , content_type string }
	files := []file{}
	start , end := "" , ""
	for _ , pair := range record {
		if pair.tag == "TY" || pair.value == "" { continue }
		rule , known := risFields[ pair.tag ]
		target := ""
		if known { target = rule.target( item_type ) }
		if target == "" {
			if rule , ok := risImportOnly[ pair.tag ]; ok { target = rule.target( item_type ) }
		}
		switch {
		case target == "" || target == "ignore":
			if target == "" { addExtra( item , pair.tag + ": " + pair.value ) }
		case strings.HasPrefix( target , "creators/" ):
			last , first := splitName( pair.value )
			addCreator( item , strings.TrimPrefix( target , "creators/" ) , last , first )
		case target == "tags":
			for _ , tag := range strings.Split( pair.value , "\n" ) { addTag( item , tag , false ) }
		case target == "notes":
			notes = append( notes , pair.value )
		case strings.HasPrefix( target , "attachments/" ):
			content_type := ""
			switch target {
			case "attachments/PDF":
				content_type = "application/pdf"
			case "attachments/HTML":
				content_type = "text/html"
			}
			for _ , location := range strings.Split( pair.value , "\n" ) {
				if location = strings.TrimSpace( location ); location != "" { files = append( files , file{ location , content_type } ) }
			}
		case target == "extra":
			addExtra( item , pair.value )
		case target == "pages" && pair.tag == "SP":
			start = pair.value
		case target == "endPage":
			end = pair.value
		case target == "date" && pair.tag == "PY" , target == "dateEnacted" && pair.tag == "PY" ,
			target == "dateDecided" && pair.tag == "PY" , target == "issueDate" && pair.tag == "PY":
			// DA is the full date; PY only a year, used when there is no DA.
			if field := TypeField( item_type , BaseField( item_type , target ) ); Str( item , field ) == "" && Str( item , target ) == "" {
				setField( item , target , risDate( pair.value ) )
			}
		case target == "date" , target == "dateEnacted" , target == "dateDecided" , target == "issueDate" , target == "accessDate":
			value := risDate( pair.value )
			field := target
			if ValidField( item_type , field ) == false { field = TypeField( item_type , BaseField( item_type , target ) ) }
			if field != "" {
				item[ field ] = value
			} else {
				setField( item , target , value )
			}
		default:
			setField( item , target , pair.value )
		}
	}
	if start != "" || end != "" {
		pages := start
		if end != "" && end != start {
			if pages != "" { pages += "-" }
			pages += end
		}
		setField( item , "pages" , pages )
	}
	item[ "key" ] = keys.make( itemSeed( item ) )
	library.Items = append( library.Items , item )
	for _ , text := range notes {
		html := text
		if strings.HasPrefix( strings.TrimSpace( text ) , "<" ) == false { html = textToHTML( text ) }
		library.Items = append( library.Items , Item{
			"itemType": "note" , "note": html , "parentItem": item[ "key" ] ,
			"key": keys.make( Str( item , "key" ) + "|note|" + html ) ,
		} )
	}
	for _ , each := range files {
		library.attach( keys , bundle , item , each.location , "" , each.content_type , "" )
	}
}

// risDate turns RIS's 2020/05/01/other into 2020-05-01.
func risDate( value string ) ( date string ) {
	parts := strings.Split( value , "/" )
	if len( parts ) < 2 || len( parts[ 0 ] ) != 4 {
		date = value
		return
	}
	date = parts[ 0 ]
	for _ , part := range parts[ 1:min( 3 , len( parts ) ) ] {
		if part == "" { break }
		if len( part ) == 1 { part = "0" + part }
		date += "-" + part
	}
	return
}

// WriteRIS writes library as RIS. pack, when it carries files, names each
// attachment's file in L1 (a PDF), L2 (HTML) or L4.
func WriteRIS( out io.Writer , library *Library , pack *packer ) ( err error ) {
	writer := bufio.NewWriterSize( out , 64 * 1024 )
	write := func( tag string , value string ) {
		value = strings.TrimSpace( value )
		if value == "" { return }
		fmt.Fprintf( writer , "%s  - %s\r\n" , tag , value )
	}
	built := buildTree( library )
	for _ , item := range built.top {
		item_type := Str( item , "itemType" )
		if item_type == "note" || item_type == "attachment" { continue }
		ty := risExportTypes[ item_type ]
		if ty == "" { ty = "GEN" }
		write( "TY" , ty )
		for _ , tag := range risExportOrder {
			target := risFields[ tag ].target( item_type )
			switch {
			case target == "":
			case strings.HasPrefix( target , "creators/" ):
				wanted := strings.TrimPrefix( target , "creators/" )
				for _ , creator := range creatorsOf( item ) {
					if Str( creator , "creatorType" ) == wanted || ( Str( creator , "creatorType" ) == "" && wanted == "author" ) {
						write( tag , creatorName( creator ) )
					}
				}
			case tag == "PY":
				if match := yearOf( itemField( item , target ) ); match != "" { write( tag , match ) }
			case tag == "DA":
				write( tag , strings.ReplaceAll( itemField( item , target ) , "-" , "/" ) )
			case tag == "SP" && target == "pages":
				pages := itemField( item , target )
				if dash := strings.IndexAny( pages , "-–" ); dash > 0 {
					write( "SP" , pages[ :dash ] )
					write( "EP" , strings.TrimLeft( pages[ dash: ] , "-–" ) )
				} else {
					write( "SP" , pages )
				}
			default:
				write( tag , itemField( item , target ) )
			}
		}
		tags , _ := tagsOf( item )
		for _ , tag := range tags { write( "KW" , tag ) }
		for _ , note := range built.notes( item ) { write( "N1" , Str( note , "note" ) ) }
		// Extra as M2, which Zotero's RIS import reads back into Extra.
		for _ , line := range strings.Split( Str( item , "extra" ) , "\n" ) { write( "M2" , line ) }
		for _ , attachment := range built.attachments( item ) {
			paths := pack.add( attachment )
			location := ""
			if len( paths ) > 0 { location = paths[ 0 ] }
			if location == "" && Str( attachment , "linkMode" ) == "linked_file" { location = Str( attachment , "path" ) }
			if location == "" {
				if link := Str( attachment , "url" ); link != "" && Str( attachment , "linkMode" ) == "linked_url" { write( "UR" , link ) }
				continue
			}
			switch content_type := Str( attachment , "contentType" ); {
			case content_type == "application/pdf":
				write( "L1" , location )
			case content_type == "text/html":
				write( "L2" , location )
			default:
				write( "L4" , location )
			}
		}
		writer.WriteString( "ER  - \r\n\r\n" )
	}
	err = writer.Flush()
	return
}

// itemField is field from item, by its own name or the base it stands for.
func itemField( item Item , field string ) ( value string ) {
	value = Str( item , field )
	if value == "" { value = get( item , BaseField( Str( item , "itemType" ) , field ) ) }
	return
}

var yearOnly = regexp.MustCompile( `\b(\d{4})\b` )

func yearOf( date string ) ( year string ) {
	year = yearOnly.FindString( date )
	return
}
