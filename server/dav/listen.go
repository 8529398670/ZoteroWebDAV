package dav

import (
	net "net"
)

func listen( address string ) ( listener net.Listener , err error ) {
	listener , err = net.Listen( "tcp" , address )
	return
}
