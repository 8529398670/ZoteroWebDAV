package dav

import (
	zip "archive/zip"
	errors "errors"
	io "io"
	path "path"
	strings "strings"
)

// A Zotero attachment on WebDAV is zotero/<KEY>.zip: everything in the
// attachment's storage/<KEY>/ folder, usually one file. The library API hands
// one file out of it straight from the zip, decompressed as it goes, so
// nothing is unpacked on disk.

var ErrNoSuchEntry = errors.New( "no such file in the attachment" )

// ZipEntry is one file in an attachment's zip.
type ZipEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Attachment is an attachment's zip, opened.
type Attachment struct {
	file    File
	reader  *zip.Reader
	Entries []ZipEntry
}

func ( attachment *Attachment ) Close() ( err error ) {
	err = attachment.file.Close()
	return
}

// OpenAttachment opens zotero/<KEY>.zip in an account's folder.
func ( server *Server ) OpenAttachment( user_id uint64 , key string ) ( attachment *Attachment , err error ) {
	file , _ , err := server.Open( user_id , "/zotero/" + key + ".zip" )
	if err != nil { return }
	reader , err := zip.NewReader( file , file.Size() )
	if err != nil {
		file.Close()
		return
	}
	attachment = &Attachment{ file: file , reader: reader , Entries: []ZipEntry{} }
	for _ , entry := range reader.File {
		if usableEntry( entry ) {
			attachment.Entries = append( attachment.Entries , ZipEntry{ Name: entry.Name , Size: int64( entry.UncompressedSize64 ) } )
		}
	}
	return
}

// usableEntry is a file Zotero would unpack: not a folder, not one of its
// own .zotero files, no path that climbs out.
func usableEntry( entry *zip.File ) ( result bool ) {
	name := entry.Name
	result = strings.HasSuffix( name , "/" ) == false &&
		strings.HasPrefix( path.Base( name ) , ".zotero" ) == false &&
		strings.HasPrefix( name , "/" ) == false &&
		strings.Contains( name , ".." ) == false
	return
}

// Main is the entry to hand out when none is named: the one wanted
// (preferred, from the item's filename), else the only one, else the largest.
func ( attachment *Attachment ) Main( preferred string ) ( name string ) {
	largest := int64( -1 )
	for _ , entry := range attachment.Entries {
		if preferred != "" && entry.Name == preferred {
			name = entry.Name
			return
		}
		if entry.Size > largest {
			name , largest = entry.Name , entry.Size
		}
	}
	return
}

// OpenEntry streams one file out of the zip.
func ( attachment *Attachment ) OpenEntry( name string ) ( reader io.ReadCloser , size int64 , err error ) {
	for _ , entry := range attachment.reader.File {
		if entry.Name != name || usableEntry( entry ) == false { continue }
		reader , err = entry.Open()
		size = int64( entry.UncompressedSize64 )
		return
	}
	err = ErrNoSuchEntry
	return
}
