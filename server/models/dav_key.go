package models

import (
	errors "errors"
	sync "sync"

	db "zoterowebdav/server/db"
	encryption "zoterowebdav/server/encryption"
)

// The WebDAV master key seals the key of every file WebDAV writes encrypted
// (webdav.encrypt_at_rest, see encryption.NewSegmentWriter). It is random,
// made the first time it is needed, and kept in the database sealed by
// secret.key's PurposeWebDAV subkey -- sealed on its own, so it is not
// readable even with server.encrypt_at_rest off, and RekeyRecord moves it
// to a new secret.key with the rest of the database. No file is rewritten
// when the secret key rotates.
//
// The cost: an encrypted file cannot be read without this database, so back
// the database up with the files.

const davMasterRecord = "master"

type davKeyRecord struct {
	SealedKey string `json:"sealed_key"`
}

var davMaster struct {
	mu  sync.Mutex
	key string
}

// DAVMasterKey is the WebDAV master key, as hex, made on first use.
func DAVMasterKey( store *db.Store , secret_key string ) ( key_hex string , err error ) {
	davMaster.mu.Lock()
	defer davMaster.mu.Unlock()
	if davMaster.key != "" {
		key_hex = davMaster.key
		return
	}
	seal := encryption.SubKey( secret_key , encryption.PurposeWebDAV )
	err = store.Write( func( tx *db.Tx ) ( tx_err error ) {
		record := &davKeyRecord{}
		tx_err = tx.Get( db.BucketDAVKeys , []byte( davMasterRecord ) , record )
		switch {
		case tx_err == nil:
			key_hex , tx_err = encryption.ChaChaDecryptBase64String( seal , record.SealedKey )
		case errors.Is( tx_err , db.ErrNotFound ):
			key_hex = encryption.GenerateKeyHex()
			if record.SealedKey , tx_err = encryption.ChaChaEncryptString( seal , key_hex ); tx_err != nil { return }
			tx_err = tx.Put( db.BucketDAVKeys , []byte( davMasterRecord ) , record )
		}
		return
	} )
	if err != nil {
		key_hex = ""
		return
	}
	davMaster.key = key_hex
	return
}
