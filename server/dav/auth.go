package dav

import (
	net "net"
	netip "net/netip"
	http "net/http"
	strings "strings"

	config "zoterowebdav/server/config"
	models "zoterowebdav/server/models"
)

// signedIn is who a request turned out to be.
type signedIn struct {
	user       *models.User
	account    *models.DAVAccount
	passwordID string
}

// authResult is how signing in went, for the handler to answer.
type authResult int

const (
	authOK authResult = iota
	// authMissing: no credentials. A challenge, nothing counted: every
	// client's first request looks like this.
	authMissing
	// authFailed: wrong name or password. Counted against the address.
	authFailed
	// authLocked: this address failed too often lately.
	authLocked
	// authOff: right credentials, but WebDAV is switched off for the account.
	authOff
)

// authenticate checks HTTP Basic credentials: the account's WebDAV name and
// one of its app passwords. Every way of being wrong is the same answer.
func ( server *Server ) authenticate( r *http.Request , address string ) ( who *signedIn , result authResult ) {
	name , password , present := r.BasicAuth()
	if present == false || password == "" {
		result = authMissing
		return
	}
	if server.failures.Reserve( address ) == false {
		result = authLocked
		return
	}
	outcome := models.AttemptFailed
	defer func() { server.failures.Settle( address , outcome ) }()

	result = authFailed
	stored , user := models.LoadDAVPassword( server.Store , password )
	if stored == nil { return }
	account , err := models.DAVAccountByName( server.Store , strings.ToLower( strings.TrimSpace( name ) ) )
	// The password has to be for this very library, not just one of the
	// same owner's.
	if err != nil || account.ID != stored.LibraryID || account.UserID != stored.UserID { return }
	outcome = models.AttemptPassed
	who = &signedIn{ user: user , account: account , passwordID: stored.ID }
	result = authOK
	if account.Enabled == false {
		result = authOff
	}
	return
}

// clientAddress is who is asking: the connection's address, or the proxy
// header's when the connection comes from a trusted proxy -- the same rule as
// the app's (server.trust_proxy, trusted_proxies, proxy_header).
func clientAddress( r *http.Request , cfg *config.Config ) ( address string ) {
	host , _ , err := net.SplitHostPort( r.RemoteAddr )
	if err != nil { host = r.RemoteAddr }
	address = host
	if cfg.TrustProxy == false { return }
	peer , err := netip.ParseAddr( host )
	if err != nil || trusted( peer.Unmap() , cfg.TrustedProxies ) == false { return }
	header := r.Header.Get( cfg.ProxyHeader )
	if header == "" { return }
	if strings.EqualFold( cfg.ProxyHeader , "X-Forwarded-For" ) {
		// Right to left, past every hop that is itself a trusted proxy.
		hops := strings.Split( header , "," )
		for index := len( hops ) - 1; index >= 0; index -= 1 {
			hop , parse_err := netip.ParseAddr( strings.TrimSpace( hops[ index ] ) )
			if parse_err != nil { return }
			address = hop.Unmap().String()
			if trusted( hop.Unmap() , cfg.TrustedProxies ) == false { return }
		}
		return
	}
	if parsed , parse_err := netip.ParseAddr( strings.TrimSpace( header ) ); parse_err == nil {
		address = parsed.Unmap().String()
	}
	return
}

func trusted( address netip.Addr , proxies []string ) ( result bool ) {
	for _ , entry := range proxies {
		entry = strings.TrimSpace( entry )
		if strings.Contains( entry , "/" ) {
			if prefix , err := netip.ParsePrefix( entry ); err == nil && prefix.Contains( address ) {
				result = true
				return
			}
			continue
		}
		if parsed , err := netip.ParseAddr( entry ); err == nil && parsed.Unmap() == address {
			result = true
			return
		}
	}
	return
}
