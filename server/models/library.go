package models

import (
	bytes "bytes"
	json "encoding/json"
	binary "encoding/binary"
	errors "errors"
	regexp "regexp"
	strings "strings"
	time "time"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
)

// The library is the metadata that goes with an account's WebDAV files.
// Zotero's WebDAV sync carries only files (zotero/<KEY>.zip and .prop), never
// titles, authors, DOIs, tags or collections, so a client that knows them
// (PRMA, reading zotero.sqlite) pushes them here, and another client with no
// Zotero beside it pulls them back.
//
// A record is Zotero's own JSON for an item or a collection (the "data"
// object of the Zotero web API), kept as it came, plus a few fields read out
// of it for listing and searching. Every change gets the account's next
// change number (Rev), and BucketLibraryRevs holds the records by it, so a
// client asks "everything since 1234" and reads only that. A delete leaves a
// tombstone for webdav.tombstone_days, so it reaches clients too.

const (
	LibraryKindItem       = "item"
	LibraryKindCollection = "collection"

	// LibraryMaxData bounds one record's JSON. Zotero's own limit on a note
	// is far below this; an item is a few KB.
	LibraryMaxData = 256 * 1024
)

var (
	ErrLibraryKey  = errors.New( "key must be a Zotero key: 8 of A-Z and 0-9" )
	ErrLibraryData = errors.New( "data must be a JSON object of at most 256 KB, and its key, if it has one, must match" )
	ErrLibraryKind = errors.New( "kind must be item or collection" )
)

// LibraryRecord is one item or collection.
type LibraryRecord struct {
	Kind      string          `json:"kind"`
	Key       string          `json:"key"`
	Version   int64           `json:"version"`
	Rev       uint64          `json:"rev"`
	Deleted   bool            `json:"deleted,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`

	// Read out of Data, for lists and search.
	Title      string   `json:"title,omitempty"`
	ItemType   string   `json:"item_type,omitempty"`
	ParentKey  string   `json:"parent_key,omitempty"`
	Creators   string   `json:"creators,omitempty"`
	Year       string   `json:"year,omitempty"`
	DOI        string   `json:"doi,omitempty"`
	Modified   string   `json:"modified,omitempty"`
	Collections []string `json:"collections,omitempty"`
	// Attachment items: the file's name and type (data.filename,
	// data.contentType).
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// LibraryInput is one record a client sends.
type LibraryInput struct {
	Key     string
	Version int64
	Data    json.RawMessage
}

// LibraryResult says what one input did: Rev is its change number, Changed
// false when it was identical to what was stored.
type LibraryResult struct {
	Key     string
	Rev     uint64
	Changed bool
}

type libraryMeta struct {
	Rev uint64 `json:"rev"`
	// PurgedRev is the newest tombstone dropped. A client whose since is
	// below it may have missed that delete, so it has to read everything.
	PurgedRev uint64 `json:"purged_rev,omitempty"`
}

type libraryRevEntry struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

func ValidLibraryKind( kind string ) ( result bool ) {
	result = kind == LibraryKindItem || kind == LibraryKindCollection
	return
}

func libraryRecordKey( user_id uint64 , kind string , key string ) ( result []byte ) {
	result = append( encryption.Uint64ToBytes( user_id ) , kind[ 0 ] )
	result = append( result , key... )
	return
}

func libraryRevKey( user_id uint64 , rev uint64 ) ( result []byte ) {
	result = encryption.Uint64ToBytes( user_id )
	result = binary.BigEndian.AppendUint64( result , rev )
	return
}

var yearPattern = regexp.MustCompile( `\b(1[5-9][0-9]{2}|2[0-9]{3})\b` )

type zoteroCreator struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Name      string `json:"name"`
}

type zoteroData struct {
	Key         string          `json:"key"`
	ItemType    string          `json:"itemType"`
	Title       string          `json:"title"`
	Name        string          `json:"name"`
	ParentItem  json.RawMessage `json:"parentItem"`
	Parent      json.RawMessage `json:"parentCollection"`
	Creators    []zoteroCreator `json:"creators"`
	Date        string          `json:"date"`
	DOI         string          `json:"DOI"`
	Modified    string          `json:"dateModified"`
	Collections []string        `json:"collections"`
	Filename    string          `json:"filename"`
	ContentType string          `json:"contentType"`
}

// Zotero writes a missing parent as false, not as an absent field.
func parentKey( raw json.RawMessage ) ( key string ) {
	if json.Unmarshal( raw , &key ) != nil { key = "" }
	return
}

// clip trims value to at most length bytes, never cutting a character.
func clip( value string , length int ) ( result string ) {
	result = strings.TrimSpace( value )
	if len( result ) > length {
		result = strings.ToValidUTF8( result[ :length ] , "" )
	}
	return
}

// describe fills the listing fields from data, which ValidLibraryData has
// already checked.
func ( record *LibraryRecord ) describe() {
	parsed := zoteroData{}
	json.Unmarshal( record.Data , &parsed )
	record.ItemType = clip( parsed.ItemType , 64 )
	record.Title = clip( parsed.Title , 500 )
	record.ParentKey = parentKey( parsed.ParentItem )
	if record.Kind == LibraryKindCollection {
		record.Title = clip( parsed.Name , 500 )
		record.ParentKey = parentKey( parsed.Parent )
	}
	names := []string{}
	for _ , creator := range parsed.Creators {
		name := creator.LastName
		if name == "" { name = creator.Name }
		if name = strings.TrimSpace( name ); name != "" { names = append( names , name ) }
	}
	switch {
	case len( names ) > 3:
		record.Creators = names[ 0 ] + " et al."
	default:
		record.Creators = strings.Join( names , ", " )
	}
	record.Creators = clip( record.Creators , 300 )
	record.Year = yearPattern.FindString( parsed.Date )
	record.DOI = clip( parsed.DOI , 200 )
	record.Modified = clip( parsed.Modified , 40 )
	record.Collections = nil
	for _ , key := range parsed.Collections {
		if ZoteroKeyPattern.MatchString( key ) { record.Collections = append( record.Collections , key ) }
	}
	record.Filename = clip( parsed.Filename , 300 )
	record.ContentType = clip( parsed.ContentType , 100 )
}

// ValidLibraryData checks one input.
func ValidLibraryData( key string , data json.RawMessage ) ( err error ) {
	if ZoteroKeyPattern.MatchString( key ) == false {
		err = ErrLibraryKey
		return
	}
	trimmed := bytes.TrimSpace( data )
	if len( trimmed ) == 0 || len( trimmed ) > LibraryMaxData || trimmed[ 0 ] != '{' || json.Valid( trimmed ) == false {
		err = ErrLibraryData
		return
	}
	parsed := struct {
		Key *string `json:"key"`
	}{}
	if json.Unmarshal( trimmed , &parsed ) != nil || ( parsed.Key != nil && *parsed.Key != key ) {
		err = ErrLibraryData
	}
	return
}

// nextRevTx hands out the account's next change number.
func nextRevTx( tx *db.Tx , user_id uint64 ) ( rev uint64 , err error ) {
	meta := &libraryMeta{}
	if get_err := tx.Get( db.BucketLibraryMeta , encryption.Uint64ToBytes( user_id ) , meta ); get_err != nil && errors.Is( get_err , db.ErrNotFound ) == false {
		err = get_err
		return
	}
	meta.Rev += 1
	rev = meta.Rev
	err = tx.Put( db.BucketLibraryMeta , encryption.Uint64ToBytes( user_id ) , meta )
	return
}

// LibraryRev is the account's last change number, what a client that has
// read everything passes as since next time, and the newest tombstone purged:
// a since below that has to start again from 0.
func LibraryRev( store *db.Store , user_id uint64 ) ( rev uint64 , purged uint64 , err error ) {
	meta := &libraryMeta{}
	err = store.Get( db.BucketLibraryMeta , encryption.Uint64ToBytes( user_id ) , meta )
	if errors.Is( err , db.ErrNotFound ) { err = nil }
	rev , purged = meta.Rev , meta.PurgedRev
	return
}

// writeRecordTx stores record under the next change number, moving its
// entry in the change index.
func writeRecordTx( tx *db.Tx , user_id uint64 , record *LibraryRecord , previous_rev uint64 ) ( err error ) {
	if previous_rev != 0 {
		if err = tx.Delete( db.BucketLibraryRevs , libraryRevKey( user_id , previous_rev ) ); err != nil { return }
	}
	if record.Rev , err = nextRevTx( tx , user_id ); err != nil { return }
	record.UpdatedAt = time.Now().UTC()
	if err = tx.Put( db.BucketLibraryRevs , libraryRevKey( user_id , record.Rev ) , &libraryRevEntry{ Kind: record.Kind , Key: record.Key } ); err != nil { return }
	err = tx.Put( db.BucketLibrary , libraryRecordKey( user_id , record.Kind , record.Key ) , record )
	return
}

// UpsertLibrary stores inputs as kind, all in one transaction, after checking
// every one. An input identical to what is stored (same version and data)
// changes nothing and gets no new change number, so a client can push its
// whole library again cheaply.
func UpsertLibrary( store *db.Store , user_id uint64 , kind string , inputs []LibraryInput ) ( results []LibraryResult , err error ) {
	if ValidLibraryKind( kind ) == false {
		err = ErrLibraryKind
		return
	}
	for _ , input := range inputs {
		if err = ValidLibraryData( input.Key , input.Data ); err != nil { return }
	}
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		results = make( []LibraryResult , 0 , len( inputs ) )
		for _ , input := range inputs {
			data := json.RawMessage( bytes.TrimSpace( input.Data ) )
			stored := &LibraryRecord{}
			previous := uint64( 0 )
			switch get_err := tx.Get( db.BucketLibrary , libraryRecordKey( user_id , kind , input.Key ) , stored ); {
			case get_err == nil:
				previous = stored.Rev
				if stored.Deleted == false && stored.Version == input.Version && bytes.Equal( stored.Data , data ) {
					results = append( results , LibraryResult{ Key: input.Key , Rev: stored.Rev } )
					continue
				}
			case errors.Is( get_err , db.ErrNotFound ) == false:
				tx_err = get_err
				return
			}
			record := &LibraryRecord{ Kind: kind , Key: input.Key , Version: input.Version , Data: data }
			record.describe()
			if tx_err = writeRecordTx( tx , user_id , record , previous ); tx_err != nil { return }
			results = append( results , LibraryResult{ Key: input.Key , Rev: record.Rev , Changed: true } )
		}
		return
	} )
	if err != nil { results = nil }
	return
}

// DeleteLibraryRecord turns a record into a tombstone. db.ErrNotFound when
// there is no live record.
func DeleteLibraryRecord( store *db.Store , user_id uint64 , kind string , key string ) ( rev uint64 , err error ) {
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		stored := &LibraryRecord{}
		if tx_err = tx.Get( db.BucketLibrary , libraryRecordKey( user_id , kind , key ) , stored ); tx_err != nil { return }
		if stored.Deleted {
			tx_err = db.ErrNotFound
			return
		}
		tombstone := &LibraryRecord{ Kind: kind , Key: key , Version: stored.Version , Deleted: true }
		if tx_err = writeRecordTx( tx , user_id , tombstone , stored.Rev ); tx_err != nil { return }
		rev = tombstone.Rev
		return
	} )
	return
}

func GetLibraryRecord( store *db.Store , user_id uint64 , kind string , key string ) ( record *LibraryRecord , err error ) {
	record = &LibraryRecord{}
	if err = store.Get( db.BucketLibrary , libraryRecordKey( user_id , kind , key ) , record ); err != nil {
		record = nil
	}
	return
}

// LibraryChanges is up to limit records changed after since, oldest change
// first, tombstones included. more says whether there are others after them.
func LibraryChanges( store *db.Store , user_id uint64 , since uint64 , limit int ) ( records []*LibraryRecord , more bool , err error ) {
	records = []*LibraryRecord{}
	entries := []*libraryRevEntry{}
	err = store.ForEachPrefix( db.BucketLibraryRevs , encryption.Uint64ToBytes( user_id ) , libraryRevKey( user_id , since + 1 ) ,
		func() any { return &libraryRevEntry{} } ,
		func( key []byte , item any ) bool {
			if len( entries ) == limit {
				more = true
				return false
			}
			entries = append( entries , item.( *libraryRevEntry ) )
			return true
		} )
	if err != nil { return }
	for _ , entry := range entries {
		record , get_err := GetLibraryRecord( store , user_id , entry.Kind , entry.Key )
		if get_err != nil {
			if errors.Is( get_err , db.ErrNotFound ) { continue }
			err = get_err
			return
		}
		records = append( records , record )
	}
	return
}

// ListLibrary is every live record of one kind, for a screen to search and
// page through.
func ListLibrary( store *db.Store , user_id uint64 , kind string ) ( records []*LibraryRecord , err error ) {
	records = []*LibraryRecord{}
	prefix := append( encryption.Uint64ToBytes( user_id ) , kind[ 0 ] )
	err = store.ForEachPrefix( db.BucketLibrary , prefix , nil ,
		func() any { return &LibraryRecord{} } ,
		func( key []byte , item any ) bool {
			if record := item.( *LibraryRecord ); record.Deleted == false {
				records = append( records , record )
			}
			return true
		} )
	return
}

// PurgeLibraryTombstones drops tombstones older than age, from every account.
func PurgeLibraryTombstones( store *db.Store , age time.Duration ) ( removed int , err error ) {
	cutoff := time.Now().UTC().Add( -age )
	type doomed struct {
		key  []byte
		rev  []byte
		user uint64
		at   uint64
	}
	found := []doomed{}
	err = store.ForEach( db.BucketLibrary ,
		func() any { return &LibraryRecord{} } ,
		func( key []byte , item any ) bool {
			if record := item.( *LibraryRecord ); record.Deleted && record.UpdatedAt.Before( cutoff ) {
				user_id := encryption.BytesToUint64( key[ :8 ] )
				found = append( found , doomed{ key: key , rev: libraryRevKey( user_id , record.Rev ) , user: user_id , at: record.Rev } )
			}
			return true
		} )
	if err != nil || len( found ) == 0 { return }
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		for _ , entry := range found {
			// A record changed since the scan is no longer this tombstone.
			current := &LibraryRecord{}
			if get_err := tx.Get( db.BucketLibrary , entry.key , current ); get_err != nil || current.Rev != entry.at { continue }
			if tx_err = tx.Delete( db.BucketLibrary , entry.key ); tx_err != nil { return }
			if tx_err = tx.Delete( db.BucketLibraryRevs , entry.rev ); tx_err != nil { return }
			meta := &libraryMeta{}
			if tx_err = tx.Get( db.BucketLibraryMeta , encryption.Uint64ToBytes( entry.user ) , meta ); tx_err != nil { return }
			meta.PurgedRev = max( meta.PurgedRev , entry.at )
			if tx_err = tx.Put( db.BucketLibraryMeta , encryption.Uint64ToBytes( entry.user ) , meta ); tx_err != nil { return }
			removed += 1
		}
		return
	} )
	if err != nil { removed = 0 }
	return
}
