// Copyright (c) 2014 The WebRTC project authors. All Rights Reserved.
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file in the root of the source
// tree.

package main

import (
	"flag"
	"log"
	"ngrtc/collider"
)

var tls = flag.Bool("tls", false, "whether TLS is used")
var port = flag.Int("port", 8080, "The TCP port that the server listens on")
var roomSrv = flag.String("room-server", "127.0.0.1", "The origin of the room server")

func main() {
	flag.Parse()

	log.Printf("Starting collider: tls = %t, port = %d, room-server=%s", *tls, *port, *roomSrv)

	c := collider.NewCollider(*roomSrv)
	c.Run(*port, *tls)
}
