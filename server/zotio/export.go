package zotio

import (
	zip "archive/zip"
	fmt "fmt"
	io "io"
	path "path"
	strings "strings"
)

// packer gives each attachment's files a place in an export with files,
// files/<n>/<name> as Zotero lays them out, and remembers them to copy. A
// nil packer, or one without files, places nothing.
type packer struct {
	files   func( key string ) []File
	entries []packEntry
	next    int
}

type packEntry struct {
	path string
	file File
}

// add places an attachment's files and says where, its main file first.
func ( pack *packer ) add( attachment Item ) ( paths []string ) {
	if pack == nil { return }
	files := pack.files( Str( attachment , "key" ) )
	if len( files ) == 0 { return }
	pack.next += 1
	main := Str( attachment , "filename" )
	ordered := []File{}
	for _ , file := range files {
		if file.Name == main { ordered = append( []File{ file } , ordered... ) } else { ordered = append( ordered , file ) }
	}
	seen := map[string]bool{}
	for _ , file := range ordered {
		name := safeName( file.Name )
		if seen[ name ] { continue }
		seen[ name ] = true
		place := fmt.Sprintf( "files/%d/%s" , pack.next , name )
		pack.entries = append( pack.entries , packEntry{ path: place , file: file } )
		paths = append( paths , place )
	}
	return
}

// safeName keeps a file name inside its folder, keeping subfolders an
// HTML snapshot has.
func safeName( name string ) ( result string ) {
	parts := []string{}
	for _ , part := range strings.Split( strings.ReplaceAll( name , "\\" , "/" ) , "/" ) {
		if part == "" || part == "." || part == ".." { continue }
		parts = append( parts , part )
	}
	result = strings.Join( parts , "/" )
	if result == "" { result = "file" }
	return
}

// Read reads an upload in format.
func Read( format string , upload *Upload ) ( library *Library , err error ) {
	switch format {
	case "rdf":
		library , err = ReadRDF( upload.Main , upload.Bundle )
	case "ris":
		library , err = ReadRIS( upload.Main , upload.Bundle )
	case "csv":
		library , err = ReadCSV( upload.Main , upload.Bundle )
	default:
		err = ErrFormat
	}
	return
}

// Export writes library as format to out. With files, out gets a zip:
// <name>.<ext> and files/<n>/... beside it, the folder Zotero's own "Export
// Files" makes, which Zotero imports with the files. Without, the bare file.
func Export( out io.Writer , format Format , library *Library , with_files bool , name string ) ( err error ) {
	write := func( target io.Writer , pack *packer ) ( write_err error ) {
		switch format.ID {
		case "rdf":
			write_err = WriteRDF( target , library , pack )
		case "ris":
			write_err = WriteRIS( target , library , pack )
		case "csv":
			write_err = WriteCSV( target , library , pack )
		default:
			write_err = ErrFormat
		}
		return
	}
	if with_files == false {
		err = write( out , nil )
		return
	}
	archive := zip.NewWriter( out )
	lookup := library.Lookup
	if lookup == nil { lookup = func( key string ) []File { return library.Files[ key ] } }
	pack := &packer{ files: lookup }
	main , err := archive.Create( name + "/" + name + "." + format.Ext )
	if err != nil { return }
	if err = write( main , pack ); err != nil { return }
	for _ , entry := range pack.entries {
		if err = copyEntry( archive , name + "/" + entry.path , entry.file ); err != nil {
			library.warn( "could not add %s: %v" , entry.path , err )
			err = nil
		}
	}
	err = archive.Close()
	return
}

func copyEntry( archive *zip.Writer , name string , file File ) ( err error ) {
	reader , err := file.Open()
	if err != nil { return }
	defer reader.Close()
	header := &zip.FileHeader{ Name: name , Method: zip.Deflate , Modified: file.ModTime }
	// PDFs and images are compressed already.
	switch strings.ToLower( path.Ext( name ) ) {
	case ".pdf" , ".jpg" , ".jpeg" , ".png" , ".gif" , ".zip" , ".epub" , ".mp3" , ".mp4" , ".webp":
		header.Method = zip.Store
	}
	writer , err := archive.CreateHeader( header )
	if err != nil { return }
	_ , err = io.Copy( writer , reader )
	return
}
