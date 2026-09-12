// Package domain holds CDAMP's core entities (Agent, Message, Thread,
// SigningKey) and the ports (InboxStore, Directory, Signer, Verifier,
// Delivery) that the app layer depends on. It is pure Go: zero I/O and
// zero imports of anything else in this module, per the dependency rule
// in 02-ARCHITECTURE.md.
package domain
