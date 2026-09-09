// Command strategon is the human CLI for the control-plane HTTP API (:8081).
//
//	strategon apply -f FILE
//	strategon get natscluster NAME
//	strategon wait natscluster NAME --for=ready
//
// Authentication is a Bearer token (--token or $STRATEGON_TOKEN).
package main
