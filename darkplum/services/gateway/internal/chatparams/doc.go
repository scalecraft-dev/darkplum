package chatparams

import "encoding/json"

// Doc is the shape of docs/chat-params.json: the registry, rendered. The
// gateway test pins the file against Document() in both directions; the api's
// apidoc command renders the API.md table from the file alone (no Go import
// between the services).
type Doc struct {
	Backends     []Backend     `json:"backends"`
	Dispositions []Disposition `json:"dispositions"`
	Excluded     DocExcluded   `json:"excluded"`
	UnknownKeys  CatchAll      `json:"unknown_keys"`
	Rows         []Row         `json:"rows"`
}

// DocExcluded states what is deliberately outside the table.
type DocExcluded struct {
	Mock string `json:"mock"`
}

// Document is the registry as the golden file records it.
func Document() Doc {
	return Doc{
		Backends:     Backends,
		Dispositions: Dispositions,
		Excluded:     DocExcluded{Mock: MockExcluded},
		UnknownKeys:  UnknownKeys,
		Rows:         rows,
	}
}

// MarshalDocument renders Document() as the golden file's exact bytes.
func MarshalDocument() ([]byte, error) {
	b, err := json.MarshalIndent(Document(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
