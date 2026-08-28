package main

import "testing"

// BenchmarkAudit runs the audit over a ~3.6M node store that grew across 2000
// versions. Building the fixture takes ~30s and is not timed.
func BenchmarkAudit(b *testing.B) {
	for _, backend := range testBackends() {
		b.Run(backend, func(b *testing.B) {
			dir := b.TempDir()
			buildVersioned(b, backend, dir, 2000, 200, 50000)
			db, names := openFixture(b, dir)
			b.ResetTimer()
			for range b.N {
				captureStdout(b, func() {
					if err := auditAll(db, names, 20); err != nil {
						b.Fatal(err)
					}
				})
			}
		})
	}
}
