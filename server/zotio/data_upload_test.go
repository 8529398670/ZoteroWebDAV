package zotio

import (
	archive_zip "archive/zip"
	sql "database/sql"
	io "io"
	os "os"
	filepath "path/filepath"
	testing "testing"
)

// tinyZotero writes a zotero.sqlite holding one journal article with a
// stored PDF, the tables the reader asks of and no more.
func tinyZotero( t *testing.T , dir string ) ( full string ) {
	t.Helper()
	full = filepath.Join( dir , "zotero.sqlite" )
	db , err := sql.Open( "sqlite" , "file:" + full )
	if err != nil { t.Fatal( err ) }
	defer db.Close()
	for _ , statement := range []string{
		`CREATE TABLE libraries ( libraryID INTEGER PRIMARY KEY , type TEXT )`,
		`CREATE TABLE collections ( collectionID INTEGER PRIMARY KEY , key TEXT , collectionName TEXT , version INT , parentCollectionID INT , libraryID INT )`,
		`CREATE TABLE deletedCollections ( collectionID INT )`,
		`CREATE TABLE items ( itemID INTEGER PRIMARY KEY , key TEXT , version INT , itemTypeID INT , dateAdded TEXT , dateModified TEXT , libraryID INT )`,
		`CREATE TABLE itemTypesCombined ( itemTypeID INT , typeName TEXT )`,
		`CREATE TABLE deletedItems ( itemID INT )`,
		`CREATE TABLE itemData ( itemID INT , fieldID INT , valueID INT )`,
		`CREATE TABLE fieldsCombined ( fieldID INT , fieldName TEXT )`,
		`CREATE TABLE itemDataValues ( valueID INT , value )`,
		`CREATE TABLE itemCreators ( itemID INT , creatorID INT , creatorTypeID INT , orderIndex INT )`,
		`CREATE TABLE creators ( creatorID INT , firstName TEXT , lastName TEXT , fieldMode INT )`,
		`CREATE TABLE creatorTypes ( creatorTypeID INT , creatorType TEXT )`,
		`CREATE TABLE itemTags ( itemID INT , tagID INT , type INT )`,
		`CREATE TABLE tags ( tagID INT , name TEXT )`,
		`CREATE TABLE collectionItems ( collectionID INT , itemID INT , orderIndex INT )`,
		`CREATE TABLE itemNotes ( itemID INT , parentItemID INT , note TEXT , title TEXT )`,
		`CREATE TABLE itemRelations ( itemID INT , predicateID INT , object TEXT )`,
		`CREATE TABLE relationPredicates ( predicateID INT , predicate TEXT )`,
		`CREATE TABLE itemAttachments ( itemID INT , parentItemID INT , linkMode INT , contentType TEXT , charsetID INT , path TEXT , storageModTime INT , storageHash TEXT )`,
		`CREATE TABLE charsets ( charsetID INT , charset TEXT )`,
		`INSERT INTO libraries VALUES ( 1 , 'user' )`,
		`INSERT INTO itemTypesCombined VALUES ( 1 , 'journalArticle' ) , ( 2 , 'attachment' )`,
		`INSERT INTO items VALUES ( 1 , 'REAL2345' , 7 , 1 , '2020-01-02 03:04:05' , '2021-02-03 04:05:06' , 1 )`,
		`INSERT INTO items VALUES ( 2 , 'FILE6789' , 3 , 2 , '2020-01-02 03:04:05' , '2020-01-02 03:04:05' , 1 )`,
		`INSERT INTO fieldsCombined VALUES ( 1 , 'title' )`,
		`INSERT INTO itemDataValues VALUES ( 1 , 'No DOI Here' )`,
		`INSERT INTO itemData VALUES ( 1 , 1 , 1 )`,
		`INSERT INTO itemAttachments VALUES ( 2 , 1 , 0 , 'application/pdf' , NULL , 'storage:paper.pdf' , 1600000000000 , 'abc123' )`,
	} {
		if _ , err = db.Exec( statement ); err != nil { t.Fatalf( "%s: %v" , statement , err ) }
	}
	return
}

// TestDataUpload: a zip of a data folder, or zotero.sqlite on its own,
// keeps Zotero's keys and dates; the zip brings each attachment's files,
// its hidden ones (the full-text cache) left out.
func TestDataUpload( t *testing.T ) {
	dir := t.TempDir()
	database , _ := os.ReadFile( tinyZotero( t , dir ) )
	zipped := filepath.Join( dir , "upload.zip" )
	out , _ := os.Create( zipped )
	writer := archive_zip.NewWriter( out )
	for name , body := range map[string][]byte{
		"Zotero/zotero.sqlite": database ,
		"Zotero/storage/FILE6789/paper.pdf": []byte( "%PDF-1.4" ) ,
		"Zotero/storage/FILE6789/.zotero-ft-cache": []byte( "text" ) ,
		"Zotero/backups/zotero.sqlite.bak": []byte( "old" ) ,
	} {
		entry , _ := writer.Create( name )
		entry.Write( body )
	}
	writer.Close()
	out.Close()

	library , closer , err := ReadDataUpload( zipped , t.TempDir() )
	if err != nil { t.Fatal( err ) }
	defer closer.Close()
	if library.OwnKeys == false { t.Error( "a data folder's keys are its own" ) }
	article := byTitle( library , "No DOI Here" )
	if Str( article , "key" ) != "REAL2345" || Str( article , "dateAdded" ) == "" { t.Fatalf( "article %v" , article ) }
	files := library.Files[ "FILE6789" ]
	if len( files ) != 1 || files[ 0 ].Name != "paper.pdf" { t.Fatalf( "files %v" , files ) }
	opened , _ := files[ 0 ].Open()
	body , _ := io.ReadAll( opened )
	opened.Close()
	if string( body ) != "%PDF-1.4" { t.Errorf( "read %q" , body ) }

	bare := filepath.Join( t.TempDir() , "upload" )
	os.WriteFile( bare , database , 0o600 )
	library , closer , err = ReadDataUpload( bare , t.TempDir() )
	if err != nil { t.Fatal( err ) }
	if closer != nil { t.Error( "nothing to close for a bare database" ) }
	if Str( byTitle( library , "No DOI Here" ) , "key" ) != "REAL2345" || len( library.Files ) != 0 { t.Errorf( "bare: %v %v" , library.Items , library.Files ) }
}
