package cloudreve

import (
	"context"
)

type UploadSession struct {
	ID            string `json:"session_id"`
	ChunkSize     int64  `json:"chunk_size"`
	URI           string `json:"uri"`
	StoragePolicy struct {
		Type string `json:"type"`
	} `json:"storage_policy"`
}

func (c *Client) CreateFolder(ctx context.Context, uri string) error {
	return c.API(ctx, "POST", "/file/create", map[string]any{
		"uri": uri, "type": "folder", "err_on_conflict": true,
	}, nil)
}

func (c *Client) StartUpload(ctx context.Context, uri string, size int64, mimeType string, previous string) (UploadSession, error) {
	input := map[string]any{"uri": uri, "size": size, "mime_type": mimeType}
	if previous != "" {
		input["entity_type"] = "version"
		input["previous"] = previous
	}
	var session UploadSession
	err := c.API(ctx, "PUT", "/file/upload", input, &session)
	return session, err
}

func (c *Client) CancelUpload(ctx context.Context, uri, id string) error {
	return c.API(ctx, "DELETE", "/file/upload", map[string]string{"id": id, "uri": uri}, nil)
}

func (c *Client) DeleteFile(ctx context.Context, uri string) error {
	return c.API(ctx, "DELETE", "/file", map[string][]string{"uris": {uri}}, nil)
}
