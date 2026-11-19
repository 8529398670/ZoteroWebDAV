package zotio

import (
	zip "archive/zip"
	bytes "bytes"
	io "io"
	os "os"
	path "path"
	filepath "path/filepath"
	strings "strings"
)

// A Zotero data folder sent as an upload, for a server that is not on the
// machine Zotero runs on: zotero.sqlite on its own (records only), or a zip
// of the data folder (zotero.sqlite, and storage/<KEY>/ for the files).
// Items keep their Zotero keys and dates, which no export carries.

// ReadDataUpload reads the upload at full. temp_dir is where the database
// is copied out to. closer, when not nil, is closed once the files have been
// read: they are read from the zip as they are copied.
func ReadDataUpload( full string , temp_dir string ) ( library *Library , closer io.Closer , err error ) {
	copy_dir , err := os.MkdirTemp( temp_dir , "zotero-db-" )
	if err != nil { return }
	defer os.RemoveAll( copy_dir )
	head := make( []byte , 4 )
	if file , open_err := os.Open( full ); open_err == nil {
		io.ReadFull( file , head )
		file.Close()
	}
	if bytes.Equal( head , []byte( "PK\x03\x04" ) ) == false {
		if err = copyFile( full , filepath.Join( copy_dir , "zotero.sqlite" ) ); err != nil { return }
		library , err = readDatabase( copy_dir , nil )
		return
	}
	reader , err := zip.OpenReader( full )
	if err != nil { return }
	defer func() {
		if err != nil { reader.Close() }
	}()
	// The shallowest zotero.sqlite: the data folder's own, not a backup
	// inside it.
	var database *zip.File
	for _ , entry := range reader.File {
		if strings.Contains( entry.Name , "__MACOSX" ) || path.Base( entry.Name ) != "zotero.sqlite" { continue }
		if database == nil || strings.Count( entry.Name , "/" ) < strings.Count( database.Name , "/" ) { database = entry }
	}
	if database == nil {
		err = ErrNoDatabase
		return
	}
	base := path.Dir( database.Name )
	prefix := "storage/"
	if base != "." { prefix = base + "/storage/" }
	// What sits beside it: a journal or WAL carries changes not yet in it.
	by_name := map[string]*zip.File{}
	storage := map[string][]File{}
	for _ , entry := range reader.File {
		by_name[ entry.Name ] = entry
		if strings.HasSuffix( entry.Name , "/" ) || strings.HasPrefix( entry.Name , prefix ) == false { continue }
		parts := strings.Split( strings.TrimPrefix( entry.Name , prefix ) , "/" )
		if len( parts ) < 2 || ValidKey( parts[ 0 ] ) == false { continue }
		hidden := false
		for _ , part := range parts[ 1: ] {
			if strings.HasPrefix( part , "." ) || part == "" { hidden = true }
		}
		if hidden { continue }
		file := zipFile( entry )
		// The path under the attachment's folder, as folderStorage gives it.
		file.Name = matchName( strings.Join( parts[ 1: ] , "/" ) )
		storage[ parts[ 0 ] ] = append( storage[ parts[ 0 ] ] , file )
	}
	for _ , suffix := range []string{ "" , "-journal" , "-wal" } {
		entry := by_name[ database.Name + suffix ]
		if entry == nil { continue }
		if err = unzipTo( entry , filepath.Join( copy_dir , "zotero.sqlite" + suffix ) ); err != nil { return }
	}
	library , err = readDatabase( copy_dir , func( key string ) []File { return storage[ key ] } )
	if err != nil { return }
	closer = reader
	return
}

func unzipTo( entry *zip.File , target string ) ( err error ) {
	in , err := entry.Open()
	if err != nil { return }
	defer in.Close()
	out , err := os.OpenFile( target , os.O_WRONLY | os.O_CREATE | os.O_EXCL , 0o600 )
	if err != nil { return }
	if _ , err = io.Copy( out , in ); err != nil {
		out.Close()
		return
	}
	err = out.Close()
	return
}
