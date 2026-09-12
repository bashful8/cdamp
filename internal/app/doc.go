// Package app holds CDAMP's use cases (SendMessage, ReceiveMessage,
// ListMessages, GetThread, SearchThreads, CreateAgent, DeliveryWorker).
// It orchestrates internal/domain entities and ports but performs no
// direct I/O itself, and imports only internal/domain, per the
// dependency rule in 02-ARCHITECTURE.md.
package app
