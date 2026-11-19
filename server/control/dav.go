package control

import (
	errors "errors"
	http "net/http"
	time "time"

	db "zoterowebdav/server/db"
	models "zoterowebdav/server/models"
)

// WebDAV over the control socket: what `manage dav-*` needs while the server
// runs. The same models calls as the WebDAV screen, so the rules are one set.

// DAVAccountResponse is an account's WebDAV folder.
type DAVAccountResponse struct {
	UserID      uint64 `json:"user_id"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	QuotaBytes  *int64 `json:"quota_bytes,omitempty"`
	UsedBytes   int64  `json:"used_bytes"`
}

// DAVPasswordResponse is an app password, with its credential when it was
// just made.
type DAVPasswordResponse struct {
	ID         string     `json:"id"`
	UserID     uint64     `json:"user_id"`
	Username   string     `json:"username"`
	Name       string     `json:"name"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Credential string     `json:"credential,omitempty"`
}

type DAVNameRequest struct {
	Name string `json:"name"`
}

type DAVPasswordRequest struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"`
}

func DAVAccountFrom( account *models.DAVAccount , display_name string ) ( result DAVAccountResponse ) {
	result = DAVAccountResponse{ UserID: account.UserID , DisplayName: display_name , Name: account.Name , Enabled: account.Enabled , QuotaBytes: account.QuotaBytes , UsedBytes: account.UsedBytes }
	return
}

// SetDAVName gives an account its WebDAV name, or renames it, the way the
// WebDAV screen does. enabled is webdav.enabled_by_default, for a new one.
func SetDAVName( store *db.Store , user_id uint64 , name string , enabled bool ) ( result DAVAccountResponse , err error ) {
	user , err := models.GetUser( store , user_id )
	if err != nil { return }
	account , _ , err := models.ClaimDAVName( store , user_id , name , enabled , true )
	if err != nil { return }
	result = DAVAccountFrom( account , user.DisplayName )
	return
}

// CreateDAVPassword makes an app password for an account.
func CreateDAVPassword( store *db.Store , user_id uint64 , name string , days int ) ( result DAVPasswordResponse , err error ) {
	if name == "" || len( name ) > 80 || days < 0 || days > 3650 {
		err = errors.New( "name must be 1-80 characters and days 0 (never) to 3650" )
		return
	}
	credential , password , err := models.IssueDAVPassword( store , user_id , name , time.Duration( days ) * 24 * time.Hour )
	if err != nil { return }
	account , err := models.GetDAVAccount( store , user_id )
	if err != nil { return }
	result = DAVPasswordResponse{ ID: password.ID , UserID: user_id , Username: account.Name , Name: password.Name , ExpiresAt: password.ExpiresAt , Credential: credential }
	return
}

// ListDAV is every account with a WebDAV folder.
func ListDAV( store *db.Store ) ( results []DAVAccountResponse , err error ) {
	accounts , err := models.ListDAVAccounts( store )
	if err != nil { return }
	users , err := models.ListUsers( store )
	if err != nil { return }
	names := map[ uint64 ]string{}
	for _ , user := range users { names[ user.ID ] = user.DisplayName }
	results = []DAVAccountResponse{}
	for _ , account := range accounts {
		results = append( results , DAVAccountFrom( account , names[ account.UserID ] ) )
	}
	return
}

func ( server *Server ) handleListDAV( w http.ResponseWriter , r *http.Request ) {
	results , err := ListDAV( server.store )
	if err != nil {
		writeError( w , http.StatusInternalServerError , err.Error() )
		return
	}
	writeJSON( w , http.StatusOK , results )
}

func ( server *Server ) handleSetDAVName( w http.ResponseWriter , r *http.Request ) {
	user_id , ok := pathUserID( w , r )
	if ok == false { return }
	body := DAVNameRequest{}
	if decodeBody( w , r , &body ) == false { return }
	result , err := SetDAVName( server.store , user_id , body.Name , server.cfg().WebDAV.EnabledByDefault )
	if err != nil {
		writeError( w , davStatus( err ) , err.Error() )
		return
	}
	writeJSON( w , http.StatusOK , result )
}

func ( server *Server ) handleCreateDAVPassword( w http.ResponseWriter , r *http.Request ) {
	user_id , ok := pathUserID( w , r )
	if ok == false { return }
	body := DAVPasswordRequest{}
	if decodeBody( w , r , &body ) == false { return }
	result , err := CreateDAVPassword( server.store , user_id , body.Name , body.ExpiresInDays )
	if err != nil {
		writeError( w , davStatus( err ) , err.Error() )
		return
	}
	writeJSON( w , http.StatusOK , result )
}

func davStatus( err error ) ( status int ) {
	status = http.StatusBadRequest
	if errors.Is( err , db.ErrNotFound ) { status = http.StatusNotFound }
	return
}

func ( client *Client ) ListDAV() ( results []DAVAccountResponse , err error ) {
	err = client.do( http.MethodGet , "/v1/webdav" , nil , &results )
	return
}

func ( client *Client ) SetDAVName( user_id uint64 , name string ) ( result DAVAccountResponse , err error ) {
	err = client.do( http.MethodPost , userPath( user_id , "name" ) , DAVNameRequest{ Name: name } , &result )
	return
}

func ( client *Client ) CreateDAVPassword( user_id uint64 , name string , days int ) ( result DAVPasswordResponse , err error ) {
	err = client.do( http.MethodPost , userPath( user_id , "passwords" ) , DAVPasswordRequest{ Name: name , ExpiresInDays: days } , &result )
	return
}

func userPath( user_id uint64 , what string ) ( result string ) {
	result = "/v1/webdav/" + uintText( user_id ) + "/" + what
	return
}
