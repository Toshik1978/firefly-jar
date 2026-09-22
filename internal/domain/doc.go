// Package domain holds the value types shared by every other package: money as an exact decimal
// (Amount, never a float) and the reconciliation window built on top of internal/civil's civil
// dates. Nothing in this package talks to Firefly III, a bank or the filesystem.
package domain
