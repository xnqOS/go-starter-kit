package example

type CreateRequest struct {
	Name string `json:"name"`
}

type UpdateRequest struct {
	Name string `json:"name"`
}

type ListParams struct {
	Limit  int
	Offset int
}

// ListResult is transport-independent; the handler adds the HTTP envelope.
type ListResult struct {
	Items  []Example
	Limit  int
	Offset int
	Total  int64
}
