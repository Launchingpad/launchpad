package notify

// ChatBodyForTest exposes the post body builder to the external tests.
func ChatBodyForTest(kind string, m ChatMessage) ([]byte, error) { return m.body(kind) }
