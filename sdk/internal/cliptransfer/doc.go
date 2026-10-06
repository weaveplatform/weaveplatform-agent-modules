// Package cliptransfer is the half of streaming clipboard transfer both ends
// share: the window a sender waits on, the sink a receiver writes a file to
// as it arrives, and the free-space check that guards the receiving disk. The
// guest's service (weaveclipboard) and the host's client (weaveclient) each
// play sender and receiver, one per direction, so the rules that decide
// whether a transfer was intact and whether it may continue exist once.
package cliptransfer
