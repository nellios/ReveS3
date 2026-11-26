package config

import (
	"path/filepath"
	"testing"
)

func TestDeploymentExamples(t *testing.T) {
	for _, tc := range []struct {
		name, listen, session, uploads string
		remote                         bool
	}{
		{"local", "127.0.0.1:7070", ".state/session.json", ".state/uploads", false},
		{"docker", "0.0.0.0:7070", "/state/session.json", "/state/uploads", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(filepath.Join("..", "..", "deploy", "gateway."+tc.name+".example.json"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Listen != tc.listen || c.AllowRemoteListen != tc.remote {
				t.Fatal("unexpected listener exposure")
			}
			if c.SessionFile != tc.session || c.UploadDir != tc.uploads {
				t.Fatal("session and upload state must use the deployment's persistent paths")
			}
			if c.BucketsRoot == "" || !c.WriteEnabled || !c.MultipartEnabled {
				t.Fatal("backup deployment requires same-name buckets and multipart writes")
			}
			if c.SingleBucketMode || len(c.Comments) < 5 || c.Comments["single_bucket_mode"] == "" || c.Comments["buckets_root"] == "" {
				t.Fatal("deployment examples must explain the mode switch and default to directory discovery")
			}
			if c.AllowHTTP || c.MaxUploadBytes != 32<<20 || c.MaxMultipartBytes != 5<<30 || c.MaxStagingBytes != 6<<30 {
				t.Fatal("deployment must retain HTTPS and bounded upload sizes")
			}
		})
	}
}
