// Command cdampd is CDAMP's single binary: it loads config, constructs
// the SQLite-backed adapters, wires them into the app-layer use cases,
// and starts the HTTP server (local agent API, federation receiver, admin
// API, and dashboard). It is the only package that imports every other
// package in this module, per 02-ARCHITECTURE.md's dependency rule.
package main
