package zotio

import (
	archive_zip "archive/zip"
	bytes "bytes"
	io "io"
	os "os"
	strings "strings"
	testing "testing"
)

// sample is a small library: a journal article with an author, tags, a
// note, a PDF attachment and a collection, and a book section.
func sample() ( library *Library ) {
	library = NewLibrary()
	library.Collections = []Item{ { "key": "COLL2345" , "name": "Reading" , "parentCollection": false } }
	library.Items = []Item{
		{
			"key": "ITEM2345" , "itemType": "journalArticle" , "title": "On Things" , "publicationTitle": "Journal of Stuff" ,
			"date": "2021-05-03" , "volume": "12" , "issue": "4" , "pages": "10-20" , "DOI": "10.1000/xyz" ,
			"url": "https://example.org/a" , "abstractNote": "Short." , "ISSN": "1234-5678" , "language": "en" ,
			"creators": []any{ map[string]any{ "creatorType": "author" , "lastName": "Smith" , "firstName": "Ada" } } ,
			"tags": []any{ map[string]any{ "tag": "alpha" } , map[string]any{ "tag": "auto" , "type": 1 } } ,
			"collections": []any{ "COLL2345" } ,
		} ,
		{ "key": "NOTE2345" , "itemType": "note" , "parentItem": "ITEM2345" , "note": "<p>A note</p>" } ,
		{
			"key": "FILE2345" , "itemType": "attachment" , "parentItem": "ITEM2345" , "linkMode": "imported_file" ,
			"title": "Full Text PDF" , "filename": "paper.pdf" , "contentType": "application/pdf" ,
		} ,
		{
			"key": "BOOK2345" , "itemType": "bookSection" , "title": "A Chapter" , "bookTitle": "The Book" ,
			"publisher": "Pub" , "place": "Town" , "date": "1999" ,
			"creators": []any{ map[string]any{ "creatorType": "editor" , "lastName": "Jones" , "firstName": "B." } } ,
		} ,
	}
	library.Files[ "FILE2345" ] = []File{ { Name: "paper.pdf" , Size: 8 , Open: func() ( io.ReadCloser , error ) {
		return io.NopCloser( strings.NewReader( "%PDF-1.4" ) ) , nil
	} } }
	return
}

func byTitle( library *Library , title string ) ( found Item ) {
	for _ , item := range library.Items {
		if Str( item , "title" ) == title { found = item }
	}
	return
}

// roundTrip exports with files, then imports the zip again.
func roundTrip( t *testing.T , id string ) ( library *Library ) {
	format , _ := FindFormat( id )
	buffer := &bytes.Buffer{}
	if err := Export( buffer , format , sample() , true , "Exported Items" ); err != nil { t.Fatal( err ) }
	path := t.TempDir() + "/upload.zip"
	os.WriteFile( path , buffer.Bytes() , 0o600 )
	upload , err := OpenUpload( path , format )
	if err != nil { t.Fatal( err ) }
	defer upload.Close()
	library , err = Read( id , upload )
	if err != nil { t.Fatal( err ) }
	return
}

func checkArticle( t *testing.T , library *Library , fields ...string ) {
	article := byTitle( library , "On Things" )
	if article == nil { t.Fatalf( "no article in %v" , library.Items ) }
	want := map[string]string{ "itemType": "journalArticle" , "publicationTitle": "Journal of Stuff" , "volume": "12" ,
		"issue": "4" , "pages": "10-20" , "DOI": "10.1000/xyz" , "url": "https://example.org/a" }
	for _ , field := range fields {
		if Str( article , field ) != want[ field ] { t.Errorf( "%s = %q, want %q" , field , Str( article , field ) , want[ field ] ) }
	}
	creators := creatorsOf( article )
	if len( creators ) != 1 || Str( creators[ 0 ] , "lastName" ) != "Smith" || Str( creators[ 0 ] , "firstName" ) != "Ada" {
		t.Errorf( "creators = %v" , creators )
	}
	pdf := 0
	for _ , item := range library.Items {
		if Str( item , "itemType" ) == "attachment" && Str( item , "parentItem" ) == Str( article , "key" ) && len( library.Files[ Str( item , "key" ) ] ) == 1 { pdf += 1 }
	}
	if pdf != 1 { t.Errorf( "attachment with its file: %d, warnings %v" , pdf , library.Warnings ) }
}

func TestRDFRoundTrip( t *testing.T ) {
	library := roundTrip( t , "rdf" )
	checkArticle( t , library , "itemType" , "publicationTitle" , "volume" , "issue" , "pages" , "DOI" , "url" )
	if section := byTitle( library , "A Chapter" ); section == nil || Str( section , "bookTitle" ) != "The Book" || Str( section , "place" ) != "Town" {
		t.Errorf( "book section = %v" , section )
	}
	if len( library.Collections ) != 1 || Str( library.Collections[ 0 ] , "name" ) != "Reading" { t.Errorf( "collections = %v" , library.Collections ) }
	if article := byTitle( library , "On Things" ); len( collectionsOf( article ) ) != 1 { t.Errorf( "article not in its collection" ) }
	notes := 0
	for _ , item := range library.Items {
		if Str( item , "itemType" ) == "note" && Str( item , "note" ) == "<p>A note</p>" { notes += 1 }
	}
	if notes != 1 { t.Errorf( "notes = %d" , notes ) }
	if Str( byTitle( library , "On Things" ) , "key" ) != "ITEM2345" { t.Errorf( "RDF did not keep the Zotero key" ) }
	again := roundTrip( t , "rdf" )
	if Str( byTitle( again , "On Things" ) , "key" ) != Str( byTitle( library , "On Things" ) , "key" ) { t.Errorf( "keys differ between imports" ) }
}

func TestRISRoundTrip( t *testing.T ) {
	library := roundTrip( t , "ris" )
	checkArticle( t , library , "itemType" , "publicationTitle" , "volume" , "issue" , "pages" , "DOI" , "url" )
	if section := byTitle( library , "A Chapter" ); section == nil || Str( section , "bookTitle" ) != "The Book" {
		t.Errorf( "book section = %v" , section )
	}
}

func TestCSVRoundTrip( t *testing.T ) {
	library := roundTrip( t , "csv" )
	checkArticle( t , library , "itemType" , "publicationTitle" , "volume" , "issue" , "pages" , "DOI" , "url" )
	if Str( byTitle( library , "On Things" ) , "key" ) != "ITEM2345" { t.Errorf( "CSV key not kept" ) }
}

func TestExportZipLayout( t *testing.T ) {
	format , _ := FindFormat( "rdf" )
	buffer := &bytes.Buffer{}
	if err := Export( buffer , format , sample() , true , "Exported Items" ); err != nil { t.Fatal( err ) }
	reader , err := archive_zip.NewReader( bytes.NewReader( buffer.Bytes() ) , int64( buffer.Len() ) )
	if err != nil { t.Fatal( err ) }
	names := []string{}
	for _ , entry := range reader.File { names = append( names , entry.Name ) }
	joined := strings.Join( names , "," )
	if joined != "Exported Items/Exported Items.rdf,Exported Items/files/1/paper.pdf" { t.Errorf( "zip holds %s" , joined ) }
}

// TestDataFolder reads a real Zotero data folder when ZOTERO_DATA names one.
func TestDataFolder( t *testing.T ) {
	folder := os.Getenv( "ZOTERO_DATA" )
	if folder == "" { t.Skip( "set ZOTERO_DATA to a Zotero data folder" ) }
	library , err := ReadDataFolder( folder , t.TempDir() , true )
	if err != nil { t.Fatal( err ) }
	t.Logf( "%d collections, %d items, %d attachments with files, %d warnings" , len( library.Collections ) , len( library.Items ) , len( library.Files ) , len( library.Warnings ) )
	if len( library.Items ) == 0 { t.Error( "no items" ) }
	for _ , item := range library.Items {
		if ValidKey( Str( item , "key" ) ) == false { t.Fatalf( "bad key %v" , item ) }
	}
}

// TestMatch: a RIS export, which has no keys, imported over the library it
// came from finds every record it stands for.
func TestMatch( t *testing.T ) {
	for _ , id := range []string{ "ris" , "csv" } {
		library := roundTrip( t , id )
		original := sample()
		Match( library , original.Items , original.Collections )
		keys := map[string]bool{}
		for _ , item := range original.Items { keys[ Str( item , "key" ) ] = true }
		for _ , item := range library.Items {
			if keys[ Str( item , "key" ) ] == false { t.Errorf( "%s: %s %q did not match" , id , Str( item , "itemType" ) , Str( item , "title" ) ) }
		}
		if len( library.Files[ "FILE2345" ] ) != 1 { t.Errorf( "%s: the file did not follow its attachment" , id ) }
	}
}

// TestBundleUnicodeForms: an export that spells a file name composed finds
// it in a zip that stored it decomposed (as macOS can), and the reverse; a
// zip that wrote a name in CP437 without the UTF-8 flag is found too.
func TestBundleUnicodeForms( t *testing.T ) {
	composed := "files/118/Önerud - 2023 - EEG.pdf"
	decomposed := "files/118/Önerud - 2023 - EEG.pdf"
	cp437 := "files/7/M\x81ller.pdf" // ü in CP437
	cases := []struct{ stored , named string ; flagless bool }{
		{ decomposed , composed , false } ,
		{ composed , decomposed , false } ,
		{ composed , "files/118/%C3%96nerud%20-%202023%20-%20EEG.pdf" , false } ,
		{ cp437 , "files/7/Müller.pdf" , true } ,
	}
	for _ , c := range cases {
		buffer := &bytes.Buffer{}
		writer := archive_zip.NewWriter( buffer )
		main , _ := writer.Create( "Exported Items.rdf" )
		main.Write( []byte( "<rdf/>" ) )
		header := &archive_zip.FileHeader{ Name: c.stored , Method: archive_zip.Store , NonUTF8: c.flagless }
		file , err := writer.CreateHeader( header )
		if err != nil { t.Fatal( err ) }
		file.Write( []byte( "%PDF-1.4" ) )
		writer.Close()
		path := t.TempDir() + "/upload.zip"
		os.WriteFile( path , buffer.Bytes() , 0o600 )
		format , _ := FindFormat( "rdf" )
		upload , err := OpenUpload( path , format )
		if err != nil { t.Fatal( err ) }
		found , ok := upload.Bundle.Find( c.named )
		upload.Close()
		if ok == false { t.Errorf( "%q not found for %q" , c.stored , c.named ); continue }
		if strings.HasSuffix( found.Name , ".pdf" ) == false || utf8Valid( found.Name ) == false { t.Errorf( "name %q" , found.Name ) }
	}
}

func utf8Valid( s string ) ( ok bool ) {
	ok = strings.ToValidUTF8( s , "�" ) == s
	return
}
