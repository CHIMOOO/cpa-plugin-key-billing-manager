package plugin

// CPA supplies request_path from its HTTP router, before scheduler selection
// and request interception. Only its explicit image endpoints are independent
// of conversational Turn State. Unknown paths, model names and Responses
// image_generation tools must retain normal State protection.
func isTurnStateImageRequest(metadata map[string]any) bool {
	path, _ := metadata[MetadataRequestPath].(string)
	return path == "/v1/images/generations" || path == "/v1/images/edits"
}
