package httpapi

import "net/http"

// listPageSize is the fixed number of items per page of every list endpoint.
const listPageSize = 100

// PageInput selects a page of a list.
type PageInput struct {
	Cursor string `query:"cursor" doc:"next_cursor of the previous page, with the same filters. Omit for the first page."`
}

// list is the response of every list endpoint.
type list[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty" doc:"Pass as cursor to read the next page; absent on the last page."`
}

// page returns the page of items after the cursor. key is a stable item identifier.
func page[T any](items []T, input PageInput, key func(T) string) (*body[list[T]], error) {
	start := 0
	if input.Cursor != "" {
		for index, item := range items {
			if key(item) == input.Cursor {
				start = index + 1
				break
			}
		}
		if start == 0 {
			return nil, fail(http.StatusUnprocessableEntity, "invalid_cursor", "cursor does not identify an item in the filtered list")
		}
	}
	end := min(start+listPageSize, len(items))
	result := list[T]{Items: items[start:end]}
	if end < len(items) {
		result.NextCursor = key(items[end-1])
	}
	if result.Items == nil {
		result.Items = []T{}
	}

	return reply(result), nil
}
