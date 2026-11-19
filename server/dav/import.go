package dav

import (
	zip "archive/zip"
	md5 "crypto/md5"
	hex "encoding/hex"
	errors "errors"
	io "io"
	path "path"
	strings "strings"
	time "time"

	disk "zoterowebdav/server/disk"
	models "zoterowebdav/server/models"
)

// An import puts attachment files into an account's folder the way Zotero's
// own WebDAV sync does: zotero/<KEY>.zip holding the attachment's files,
// then zotero/<KEY>.prop with the main file's mtime and md5. Both go through
// the same writes as a WebDAV PUT, so the quota, encryption at rest, the
// atomic rename and the attachment index all apply.

// SourceFile is one file of an attachment, opened when it is copied.
type SourceFile struct {
	Name    string
	ModTime time.Time
	Open    func() ( io.ReadCloser , error )
}

// ErrDiskFull is the disk below webdav.min_free_mb.
var ErrDiskFull = errors.New( "the server is out of space" )

// PutAttachment stores an attachment's files under key. main names the file
// the .prop describes (the attachment's filename; the first file when it is
// not among them). md5_hint, when given, is that file's md5 as Zotero
// recorded it: an attachment already stored with it is left alone (skipped).
func ( server *Server ) PutAttachment( user_id uint64 , key string , main string , files []SourceFile , md5_hint string , mtime_hint int64 ) ( skipped bool , err error ) {
	if models.ZoteroKeyPattern.MatchString( key ) == false || len( files ) == 0 {
		err = errors.New( "nothing to store" )
		return
	}
	main_index := 0
	for index , file := range files {
		if file.Name == main { main_index = index }
	}
	md5_hint = strings.ToLower( md5_hint )
	if md5_hint == "" {
		if md5_hint , err = fileMD5( files[ main_index ] ); err != nil { return }
	}
	if existing , get_err := models.GetDAVAttachment( server.Store , user_id , key ); get_err == nil && existing.HasZip && existing.HasProp && existing.MD5 == md5_hint {
		skipped = true
		return
	}
	cfg := server.cfg()
	if cfg.WebDAV.MinFreeBytes > 0 {
		if free , _ , space_err := disk.Space( cfg.WebDAV.Dir ); space_err == nil && int64( free ) < cfg.WebDAV.MinFreeBytes {
			err = ErrDiskFull
			return
		}
	}
	state := server.state( user_id )
	if err = server.ensureRoot( user_id , state ); err != nil { return }
	fs := server.fsFor( user_id )
	if err = fs.putFile( "/zotero/" + key + ".zip" , func( out io.Writer ) error { return zipFiles( out , files ) } ); err != nil { return }
	mtime := mtime_hint
	if mtime <= 0 { mtime = files[ main_index ].ModTime.UnixMilli() }
	prop := models.ZoteroProp( mtime , md5_hint )
	err = fs.putFile( "/zotero/" + key + ".prop" , func( out io.Writer ) ( write_err error ) {
		_ , write_err = out.Write( prop )
		return
	} )
	return
}

// putFile writes one file whole through the WebDAV write path.
func ( fs *userFS ) putFile( name string , fill func( io.Writer ) error ) ( err error ) {
	full , rel , err := fs.resolve( name )
	if err != nil { return }
	file , err := fs.newWriteFile( full , rel , nil )
	if err != nil { return }
	if err = fill( file ); err != nil {
		file.closed = true
		file.abort()
		return
	}
	err = file.Close()
	return
}

func fileMD5( file SourceFile ) ( sum string , err error ) {
	reader , err := file.Open()
	if err != nil { return }
	defer reader.Close()
	hash := md5.New()
	if _ , err = io.Copy( hash , reader ); err != nil { return }
	sum = hex.EncodeToString( hash.Sum( nil ) )
	return
}

// zipFiles writes the zip Zotero makes of an attachment's folder.
func zipFiles( out io.Writer , files []SourceFile ) ( err error ) {
	archive := zip.NewWriter( out )
	for _ , file := range files {
		header := &zip.FileHeader{ Name: file.Name , Method: zip.Deflate , Modified: file.ModTime }
		switch strings.ToLower( path.Ext( file.Name ) ) {
		case ".pdf" , ".jpg" , ".jpeg" , ".png" , ".gif" , ".zip" , ".epub" , ".mp3" , ".mp4" , ".webp":
			header.Method = zip.Store
		}
		writer , create_err := archive.CreateHeader( header )
		if create_err != nil {
			err = create_err
			return
		}
		reader , open_err := file.Open()
		if open_err != nil {
			err = open_err
			return
		}
		_ , err = io.Copy( writer , reader )
		reader.Close()
		if err != nil { return }
	}
	err = archive.Close()
	return
}
