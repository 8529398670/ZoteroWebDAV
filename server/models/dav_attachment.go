package models

import (
	bytes "bytes"
	xml "encoding/xml"
	errors "errors"
	regexp "regexp"
	sort "sort"
	strconv "strconv"
	strings "strings"
	time "time"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
)

// DAVAttachment is what one account's WebDAV folder holds for one Zotero
// attachment: zotero/<KEY>.zip, the attachment's files, and zotero/<KEY>.prop,
// the main file's mtime and md5. Zotero writes the zip first and the .prop
// last, so a .prop says the zip beside it is whole.
//
// The WebDAV server keeps this index as files land and go (server/dav), and
// dav-reconcile rebuilds it from the folder. It is what lets a client ask
// "which of these do you already have" without listing the folder.
type DAVAttachment struct {
	Key       string    `json:"key"`
	MTime     int64     `json:"mtime,omitempty"`
	MD5       string    `json:"md5,omitempty"`
	HasProp   bool      `json:"has_prop"`
	ZipSize   int64     `json:"zip_size,omitempty"`
	HasZip    bool      `json:"has_zip"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ZoteroKeyPattern is a Zotero item key: eight of A-Z and 0-9.
var ZoteroKeyPattern = regexp.MustCompile( `^[A-Z0-9]{8}$` )

var md5Pattern = regexp.MustCompile( `^[0-9a-f]{32}$` )

// ErrPropInvalid is a .prop file that is not what Zotero writes.
var ErrPropInvalid = errors.New( "not a Zotero .prop file" )

type zoteroProp struct {
	XMLName xml.Name `xml:"properties"`
	MTime   string   `xml:"mtime"`
	Hash    string   `xml:"hash"`
}

// ParseZoteroProp reads a .prop file:
// <properties version="1"><mtime>ms</mtime><hash>md5</hash></properties>.
// Zotero once wrote seconds rather than milliseconds, and still reads them.
func ParseZoteroProp( raw []byte ) ( mtime int64 , md5 string , err error ) {
	prop := zoteroProp{}
	if xml.NewDecoder( bytes.NewReader( raw ) ).Decode( &prop ) != nil {
		err = ErrPropInvalid
		return
	}
	text := strings.TrimSpace( prop.MTime )
	md5 = strings.ToLower( strings.TrimSpace( prop.Hash ) )
	if len( text ) < 1 || len( text ) > 13 || md5Pattern.MatchString( md5 ) == false {
		err = ErrPropInvalid
		return
	}
	if mtime , err = strconv.ParseInt( text , 10 , 64 ); err != nil || mtime < 0 {
		err = ErrPropInvalid
	}
	return
}

// ZoteroProp writes a .prop file, for a client building one.
func ZoteroProp( mtime int64 , md5 string ) ( raw []byte ) {
	raw = []byte( `<properties version="1"><mtime>` + strconv.FormatInt( mtime , 10 ) + `</mtime><hash>` + md5 + `</hash></properties>` )
	return
}

func attachmentKey( library_id uint64 , key string ) ( result []byte ) {
	result = append( encryption.Uint64ToBytes( library_id ) , key... )
	return
}

// changeDAVAttachment edits one index row, making it when change finds none,
// and drops it once it has neither file.
func changeDAVAttachment( store *db.Store , library_id uint64 , key string , change func( row *DAVAttachment ) ) ( err error ) {
	if ZoteroKeyPattern.MatchString( key ) == false { return }
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		row := &DAVAttachment{}
		if get_err := tx.Get( db.BucketDAVAttachments , attachmentKey( library_id , key ) , row ); get_err != nil {
			if errors.Is( get_err , db.ErrNotFound ) == false {
				tx_err = get_err
				return
			}
			row = &DAVAttachment{ Key: key }
		}
		change( row )
		row.UpdatedAt = time.Now().UTC()
		if row.HasProp == false && row.HasZip == false {
			tx_err = tx.Delete( db.BucketDAVAttachments , attachmentKey( library_id , key ) )
			return
		}
		tx_err = tx.Put( db.BucketDAVAttachments , attachmentKey( library_id , key ) , row )
		return
	} )
	return
}

// NoteDAVProp records a .prop written; a .prop that does not parse counts as
// no .prop, as it does to Zotero, which deletes it.
func NoteDAVProp( store *db.Store , library_id uint64 , key string , raw []byte ) ( err error ) {
	mtime , md5 , parse_err := ParseZoteroProp( raw )
	err = changeDAVAttachment( store , library_id , key , func( row *DAVAttachment ) {
		row.HasProp = parse_err == nil
		row.MTime , row.MD5 = mtime , md5
	} )
	return
}

func NoteDAVZip( store *db.Store , library_id uint64 , key string , size int64 ) ( err error ) {
	err = changeDAVAttachment( store , library_id , key , func( row *DAVAttachment ) {
		row.HasZip , row.ZipSize = true , size
	} )
	return
}

func ForgetDAVProp( store *db.Store , library_id uint64 , key string ) ( err error ) {
	err = changeDAVAttachment( store , library_id , key , func( row *DAVAttachment ) {
		row.HasProp , row.MTime , row.MD5 = false , 0 , ""
	} )
	return
}

func ForgetDAVZip( store *db.Store , library_id uint64 , key string ) ( err error ) {
	err = changeDAVAttachment( store , library_id , key , func( row *DAVAttachment ) {
		row.HasZip , row.ZipSize = false , 0
	} )
	return
}

func GetDAVAttachment( store *db.Store , library_id uint64 , key string ) ( row *DAVAttachment , err error ) {
	row = &DAVAttachment{}
	if err = store.Get( db.BucketDAVAttachments , attachmentKey( library_id , key ) , row ); err != nil {
		row = nil
	}
	return
}

// ListDAVAttachments is one account's index, by key.
func ListDAVAttachments( store *db.Store , library_id uint64 ) ( rows []*DAVAttachment , err error ) {
	rows = []*DAVAttachment{}
	err = store.ForEachPrefix( db.BucketDAVAttachments , encryption.Uint64ToBytes( library_id ) , nil ,
		func() any { return &DAVAttachment{} } ,
		func( key []byte , item any ) bool {
			rows = append( rows , item.( *DAVAttachment ) )
			return true
		} )
	return
}

// ReplaceDAVAttachments makes the index exactly rows, in one transaction:
// dav-reconcile's answer after reading the folder itself.
func ReplaceDAVAttachments( store *db.Store , library_id uint64 , rows []*DAVAttachment ) ( err error ) {
	sort.Slice( rows , func( i int , j int ) bool { return rows[ i ].Key < rows[ j ].Key } )
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		if tx_err = tx.DeletePrefix( db.BucketDAVAttachments , encryption.Uint64ToBytes( library_id ) ); tx_err != nil { return }
		for _ , row := range rows {
			if tx_err = tx.Put( db.BucketDAVAttachments , attachmentKey( library_id , row.Key ) , row ); tx_err != nil { return }
		}
		return
	} )
	return
}
