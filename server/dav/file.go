package dav

import (
	bufio "bufio"
	errors "errors"
	fmt "fmt"
	io "io"
	io_fs "io/fs"
	os "os"
	filepath "path/filepath"
	strings "strings"
	time "time"

	encryption "zoterowebdav/server/encryption"
)

var (
	errTooLarge   = errors.New( "file is larger than webdav.max_file_mb" )
	errQuota      = errors.New( "the WebDAV folder is full" )
	errIncomplete = errors.New( "the upload ended before the whole file arrived" )
	errReadOnly   = errors.New( "file is open for reading" )
	errWriteOnly  = errors.New( "file is open for writing" )
)

// headKeep is how much of a written file is kept in memory for the
// attachment index: a .prop is about a hundred bytes.
const headKeep = 4096

// sizedInfo is a file's FileInfo with its plaintext size, for an encrypted
// file whose size on disk is larger.
type sizedInfo struct {
	os.FileInfo
	size int64
}

func ( info sizedInfo ) Size() ( size int64 ) {
	size = info.size
	return
}

// plainInfo is info as a client should see it: an encrypted file at its
// plaintext size. Only a file whose size a segmented file could have, and
// whose first bytes say it is one, is read.
func plainInfo( full string , info os.FileInfo ) ( result os.FileInfo ) {
	result = info
	if info.Mode().IsRegular() == false { return }
	plain , ok := encryption.SegmentedPlainSize( info.Size() )
	if ok == false { return }
	handle , err := os.Open( full )
	if err != nil { return }
	defer handle.Close()
	head := make( []byte , 8 )
	if _ , err = io.ReadFull( handle , head ); err != nil { return }
	if encryption.IsSegmented( head ) {
		result = sizedInfo{ FileInfo: info , size: plain }
	}
	return
}

// openRead opens a file or folder for reading.
func ( server *Server ) openRead( full string , rel string ) ( file *readFile , err error ) {
	handle , err := os.Open( full )
	if err != nil { return }
	info , err := handle.Stat()
	if err != nil {
		handle.Close()
		return
	}
	file = &readFile{ handle: handle , full: full , info: info }
	if info.Mode().IsRegular() == false { return }
	head := make( []byte , 8 )
	if read , _ := handle.ReadAt( head , 0 ); encryption.IsSegmented( head[ :read ] ) == false { return }
	master , err := server.MasterKey()
	if err != nil {
		handle.Close()
		file = nil
		return
	}
	segments , open_err := encryption.OpenSegments( handle , info.Size() , master )
	if open_err != nil {
		// Starts like one of ours but is not: someone else's encrypted file,
		// stored as it came.
		return
	}
	file.segments = segments
	file.info = sizedInfo{ FileInfo: info , size: segments.Size() }
	return
}

// readFile is a file or folder opened for reading, decrypted when it is
// encrypted.
type readFile struct {
	handle   *os.File
	full     string
	info     os.FileInfo
	segments *encryption.SegmentReader
	offset   int64
}

func ( file *readFile ) Close() ( err error ) {
	err = file.handle.Close()
	return
}

func ( file *readFile ) Read( data []byte ) ( read int , err error ) {
	if file.segments == nil {
		read , err = file.handle.Read( data )
		return
	}
	read , err = file.segments.ReadAt( data , file.offset )
	file.offset += int64( read )
	if err == io.EOF && read > 0 { err = nil }
	return
}

// ReadAt lets archive/zip read a zip in place.
func ( file *readFile ) ReadAt( data []byte , offset int64 ) ( read int , err error ) {
	if file.segments == nil {
		read , err = file.handle.ReadAt( data , offset )
		return
	}
	read , err = file.segments.ReadAt( data , offset )
	return
}

func ( file *readFile ) Seek( offset int64 , whence int ) ( position int64 , err error ) {
	if file.segments == nil {
		position , err = file.handle.Seek( offset , whence )
		return
	}
	switch whence {
	case io.SeekStart:
		position = offset
	case io.SeekCurrent:
		position = file.offset + offset
	case io.SeekEnd:
		position = file.segments.Size() + offset
	default:
		err = errors.New( "seek: bad whence" )
		return
	}
	if position < 0 {
		err = errors.New( "seek: negative position" )
		return
	}
	file.offset = position
	return
}

func ( file *readFile ) Size() ( size int64 ) {
	size = file.info.Size()
	return
}

// Readdir lists a folder as a client should see it: no reserved files, no
// links, encrypted files at their plaintext size.
func ( file *readFile ) Readdir( count int ) ( infos []io_fs.FileInfo , err error ) {
	entries , err := file.handle.Readdir( count )
	infos = make( []io_fs.FileInfo , 0 , len( entries ) )
	for _ , entry := range entries {
		if strings.HasPrefix( entry.Name() , reservedPrefix ) { continue }
		if entry.Mode().IsDir() == false && entry.Mode().IsRegular() == false { continue }
		infos = append( infos , plainInfo( filepath.Join( file.full , entry.Name() ) , entry ) )
	}
	return
}

func ( file *readFile ) Stat() ( info os.FileInfo , err error ) {
	info = file.info
	return
}

func ( file *readFile ) Write( data []byte ) ( written int , err error ) {
	err = errReadOnly
	return
}

// writeFile is a file being written. Bytes go to a hidden file in the same
// folder, encrypted when webdav.encrypt_at_rest says so, and Close renames it
// over the target -- unless the upload did not arrive whole, broke a limit,
// or failed, in which case Close removes it and the old file stays.
type writeFile struct {
	fs        *userFS
	full      string
	rel       string
	temp      *os.File
	buffered  *bufio.Writer
	segments  *encryption.SegmentWriter
	sink      io.Writer
	put       *putState
	written   int64
	allowance int64
	maxSize   int64
	head      []byte
	failed    error
	closed    bool
	started   time.Time
}

func ( fs *userFS ) newWriteFile( full string , rel string , put *putState ) ( file *writeFile , err error ) {
	cfg := fs.server.cfg()
	file = &writeFile{ fs: fs , full: full , rel: rel , put: put , maxSize: cfg.WebDAV.MaxFileBytes , allowance: -1 , started: time.Now() }
	if account , account_err := fs.server.accountQuota( fs.userID ); account_err == nil && account > 0 {
		file.allowance = account - fs.server.usedBytes( fs.userID , fs.state )
		if existing , stat_err := os.Lstat( full ); stat_err == nil { file.allowance += existing.Size() }
	}
	temp , err := os.CreateTemp( filepath.Dir( full ) , reservedPrefix + "upload-*" )
	if err != nil {
		file = nil
		return
	}
	file.temp = temp
	file.buffered = bufio.NewWriterSize( temp , 256 * 1024 )
	file.sink = file.buffered
	if cfg.WebDAV.EncryptAtRest {
		master , key_err := fs.server.MasterKey()
		if key_err == nil {
			file.segments , key_err = encryption.NewSegmentWriter( file.buffered , master )
		}
		if key_err != nil {
			file.abort()
			file , err = nil , key_err
			return
		}
		file.sink = file.segments
	}
	return
}

func ( file *writeFile ) Write( data []byte ) ( written int , err error ) {
	if file.failed != nil {
		err = file.failed
		return
	}
	next := file.written + int64( len( data ) )
	switch {
	case next > file.maxSize:
		file.failed = errTooLarge
	case file.allowance >= 0 && next > file.allowance:
		file.failed = errQuota
	}
	if file.failed != nil {
		err = file.failed
		return
	}
	if room := headKeep - len( file.head ); room > 0 {
		file.head = append( file.head , data[ :min( room , len( data ) ) ]... )
	}
	written , err = file.sink.Write( data )
	file.written += int64( written )
	if err != nil { file.failed = err }
	return
}

func ( file *writeFile ) abort() {
	file.temp.Close()
	os.Remove( file.temp.Name() )
}

// Close puts the file in place, or throws it away.
func ( file *writeFile ) Close() ( err error ) {
	if file.closed { return }
	file.closed = true
	switch {
	case file.failed != nil:
		err = file.failed
	case file.put != nil && file.put.complete == false:
		err = errIncomplete
	}
	if err == nil && file.segments != nil { err = file.segments.Close() }
	if err == nil { err = file.buffered.Flush() }
	if err == nil { err = file.temp.Sync() }
	if err != nil {
		file.abort()
		return
	}
	info , err := file.temp.Stat()
	if err != nil {
		file.abort()
		return
	}
	if err = file.temp.Close(); err != nil {
		os.Remove( file.temp.Name() )
		return
	}
	previous := int64( 0 )
	if existing , stat_err := os.Lstat( file.full ); stat_err == nil && existing.Mode().IsRegular() {
		previous = existing.Size()
	}
	if err = os.Rename( file.temp.Name() , file.full ); err != nil {
		os.Remove( file.temp.Name() )
		err = fmt.Errorf( "could not put the file in place: %w" , err )
		return
	}
	file.fs.server.addUsage( file.fs.userID , file.fs.state , info.Size() - previous )
	file.fs.noteWritten( file.rel , file.written , file.head )
	return
}

func ( file *writeFile ) Read( data []byte ) ( read int , err error ) {
	err = errWriteOnly
	return
}

func ( file *writeFile ) Seek( offset int64 , whence int ) ( position int64 , err error ) {
	err = errWriteOnly
	return
}

func ( file *writeFile ) Readdir( count int ) ( infos []io_fs.FileInfo , err error ) {
	err = errWriteOnly
	return
}

// Stat is what x/net/webdav makes the ETag from, before Close.
func ( file *writeFile ) Stat() ( info os.FileInfo , err error ) {
	info = writtenInfo{ name: filepath.Base( file.full ) , size: file.written , modified: time.Now() }
	return
}

type writtenInfo struct {
	name     string
	size     int64
	modified time.Time
}

func ( info writtenInfo ) Name() ( result string ) { result = info.name; return }
func ( info writtenInfo ) Size() ( result int64 ) { result = info.size; return }
func ( info writtenInfo ) Mode() ( result os.FileMode ) { result = 0o600; return }
func ( info writtenInfo ) ModTime() ( result time.Time ) { result = info.modified; return }
func ( info writtenInfo ) IsDir() ( result bool ) { return }
func ( info writtenInfo ) Sys() ( result any ) { return }
