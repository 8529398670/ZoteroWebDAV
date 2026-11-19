package models

import (
	errors "errors"
	sort "sort"
	strings "strings"
	time "time"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
)

// DAVPassword is an app password: what a WebDAV client (Zotero, Finder,
// rclone, PRMA) signs in with, as the password beside the account's WebDAV
// name. It opens that one account's WebDAV folder and nothing else -- never
// the API, never a session -- because it ends up saved in other programs'
// settings, where it is easier to lose than a browser cookie.
//
// It is an API key in all but reach: "<id>.<secret>", the id the bolt key,
// sha256 of a 256-bit random secret compared in constant time, shown once,
// revocable, soft-deleted for a short undo. Making and managing one needs a
// browser session, as for an API key, so one credential cannot mint another.
type DAVPassword struct {
	ID         string     `json:"id"`
	UserID     uint64     `json:"user_id"`
	// LibraryID is the one WebDAV library this password opens.
	LibraryID  uint64     `json:"library_id"`
	Name       string     `json:"name"`
	SecretHash string     `json:"secret_hash"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	SoftDelete
}

var (
	ErrDAVPasswordLimit    = errors.New( "this account already holds the maximum number of live app passwords" )
	ErrDAVPasswordNoFolder = errors.New( "choose a WebDAV name before making an app password" )
)

const (
	DAVPasswordMaxPerUser      = 25
	davPasswordMaxRowsPerUser  = 100
	davPasswordTouchInterval   = 15 * time.Minute
	davPasswordRetention       = 30 * 24 * time.Hour
)

func ( password *DAVPassword ) Revoked() ( result bool ) {
	result = password.RevokedAt != nil
	return
}

func ( password *DAVPassword ) Expired() ( result bool ) {
	result = password.ExpiresAt != nil && time.Now().UTC().After( *password.ExpiresAt )
	return
}

func ( password *DAVPassword ) Live() ( result bool ) {
	result = password.Revoked() == false && password.Expired() == false && password.Deleted() == false
	return
}

// IssueDAVPassword returns the credential exactly once. It opens library_id,
// which has to be one of user_id's. ttl 0 never expires.
func IssueDAVPassword( store *db.Store , user_id uint64 , library_id uint64 , name string , ttl time.Duration ) ( credential string , password *DAVPassword , err error ) {
	password_id := encryption.GenerateURLToken( 12 )
	secret := encryption.GenerateURLToken( 32 )
	now := time.Now().UTC()
	password = &DAVPassword{
		ID:         password_id,
		UserID:     user_id,
		LibraryID:  library_id,
		Name:       strings.TrimSpace( name ),
		SecretHash: encryption.Sha256Hex( secret ),
		CreatedAt:  now,
	}
	if ttl > 0 {
		expires := now.Add( ttl )
		password.ExpiresAt = &expires
	}
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		owner := &User{}
		if tx_err = tx.Get( db.BucketUsers , encryption.Uint64ToBytes( user_id ) , owner ); tx_err != nil { return }
		if owner.Disabled() {
			tx_err = ErrAPIKeyDisabled
			return
		}
		library := &DAVAccount{}
		tx_err = tx.Get( db.BucketDAVAccounts , encryption.Uint64ToBytes( library_id ) , library )
		if errors.Is( tx_err , db.ErrNotFound ) || ( tx_err == nil && library.UserID != user_id ) {
			tx_err = ErrDAVPasswordNoFolder
			return
		}
		if tx_err != nil { return }
		owned := []*DAVPassword{}
		tx_err = tx.ForEach( db.BucketDAVPasswords ,
			func() any { return &DAVPassword{} } ,
			func( key []byte , item any ) bool {
				if stored := item.( *DAVPassword ); stored.UserID == user_id {
					owned = append( owned , stored )
				}
				return true
			} )
		if tx_err != nil { return }
		live := 0
		dead := []*DAVPassword{}
		for _ , stored := range owned {
			if stored.Live() {
				live += 1
			} else {
				dead = append( dead , stored )
			}
		}
		if live >= DAVPasswordMaxPerUser {
			tx_err = ErrDAVPasswordLimit
			return
		}
		sort.Slice( dead , func( i int , j int ) bool { return dead[ i ].CreatedAt.Before( dead[ j ].CreatedAt ) } )
		for excess := len( owned ) + 1 - davPasswordMaxRowsPerUser; excess > 0 && len( dead ) > 0; excess -= 1 {
			if tx_err = tx.Delete( db.BucketDAVPasswords , []byte( dead[ 0 ].ID ) ); tx_err != nil { return }
			dead = dead[ 1: ]
		}
		tx_err = tx.Put( db.BucketDAVPasswords , []byte( password_id ) , password )
		return
	} )
	if err != nil {
		password = nil
		return
	}
	credential = password_id + "." + secret
	return
}

// LoadDAVPassword returns nil for anything that is not a usable app password:
// wrong shape, unknown, wrong secret, dead, or its account disabled. One
// answer for every failure, so nothing says which check failed.
func LoadDAVPassword( store *db.Store , credential string ) ( password *DAVPassword , user *User ) {
	password_id , secret , ok := splitCredential( credential )
	if ok == false { return }
	found := &DAVPassword{}
	if err := store.Get( db.BucketDAVPasswords , []byte( password_id ) , found ); err != nil {
		// Spend the same time on an unknown id as on a known one.
		encryption.ConstantTimeEqual( encryption.Sha256Hex( secret ) , encryption.Sha256Hex( password_id ) )
		return
	}
	if encryption.ConstantTimeEqual( found.SecretHash , encryption.Sha256Hex( secret ) ) == false { return }
	if found.Live() == false { return }
	loaded_user , err := GetUser( store , found.UserID )
	if err != nil || loaded_user.Disabled() { return }
	touchDAVPassword( store , found )
	password , user = found , loaded_user
	return
}

func touchDAVPassword( store *db.Store , password *DAVPassword ) {
	now := time.Now().UTC()
	if password.LastUsedAt != nil && now.Sub( *password.LastUsedAt ) < davPasswordTouchInterval { return }
	store.UpdateValue( db.BucketDAVPasswords , []byte( password.ID ) ,
		func() any { return &DAVPassword{} } ,
		func( item any ) ( mutate_err error ) {
			item.( *DAVPassword ).LastUsedAt = &now
			return
		} )
	password.LastUsedAt = &now
}

// ListDAVPasswordsForUser is one account's app passwords, newest first,
// deleted ones left out.
func ListDAVPasswordsForUser( store *db.Store , user_id uint64 ) ( passwords []*DAVPassword , err error ) {
	passwords = []*DAVPassword{}
	err = store.ForEach( db.BucketDAVPasswords ,
		func() any { return &DAVPassword{} } ,
		func( key []byte , item any ) bool {
			if stored := item.( *DAVPassword ); stored.UserID == user_id && stored.Deleted() == false {
				passwords = append( passwords , stored )
			}
			return true
		} )
	sort.SliceStable( passwords , func( i int , j int ) bool { return passwords[ i ].CreatedAt.After( passwords[ j ].CreatedAt ) } )
	return
}

// changeOwnDAVPassword applies change to one of user_id's app passwords.
// Someone else's is db.ErrNotFound, the same as one that does not exist.
func changeOwnDAVPassword( store *db.Store , password_id string , user_id uint64 , change func( password *DAVPassword ) error ) ( password *DAVPassword , err error ) {
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		password = &DAVPassword{}
		if tx_err = tx.Get( db.BucketDAVPasswords , []byte( password_id ) , password ); tx_err != nil { return }
		if password.UserID != user_id {
			tx_err = db.ErrNotFound
			return
		}
		if tx_err = change( password ); tx_err != nil { return }
		tx_err = tx.Put( db.BucketDAVPasswords , []byte( password_id ) , password )
		return
	} )
	if err != nil { password = nil }
	return
}

// RevokeDAVPassword stops one of your app passwords for good.
func RevokeDAVPassword( store *db.Store , password_id string , user_id uint64 ) ( password *DAVPassword , err error ) {
	password , err = changeOwnDAVPassword( store , password_id , user_id , func( password *DAVPassword ) ( change_err error ) {
		if password.Deleted() {
			change_err = db.ErrNotFound
			return
		}
		if password.RevokedAt == nil {
			now := time.Now().UTC()
			password.RevokedAt = &now
		}
		return
	} )
	return
}

// DeleteDAVPassword takes one of your app passwords away; it stops at once,
// and RestoreDAVPassword can bring it back for RestoreWindow.
func DeleteDAVPassword( store *db.Store , password_id string , user_id uint64 ) ( password *DAVPassword , err error ) {
	password , err = changeOwnDAVPassword( store , password_id , user_id , func( password *DAVPassword ) ( change_err error ) {
		password.markDeleted( user_id )
		return
	} )
	return
}

func RestoreDAVPassword( store *db.Store , password_id string , user_id uint64 ) ( password *DAVPassword , err error ) {
	password , err = changeOwnDAVPassword( store , password_id , user_id , func( password *DAVPassword ) ( change_err error ) {
		change_err = password.unmarkDeleted( user_id , false , 0 )
		return
	} )
	return
}

// PurgeDeadDAVPasswords drops rows deleted past their undo, and revoked or
// expired ones kept for davPasswordRetention.
func PurgeDeadDAVPasswords( store *db.Store ) ( removed int , err error ) {
	now := time.Now().UTC()
	removed , err = store.DeleteWhere( db.BucketDAVPasswords ,
		func() any { return &DAVPassword{} } ,
		func( key []byte , item any ) bool {
			password := item.( *DAVPassword )
			switch {
			case password.deletedLongAgo():
				return true
			case password.RevokedAt != nil && now.Sub( *password.RevokedAt ) > davPasswordRetention:
				return true
			case password.ExpiresAt != nil && now.Sub( *password.ExpiresAt ) > davPasswordRetention:
				return true
			}
			return false
		} )
	return
}
