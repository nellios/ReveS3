package gateway

import (
	"net/url"
	"strconv"
	"strings"

	"cloudreve-s3/internal/config"
	"github.com/gofiber/fiber/v3"
	"github.com/versity/versitygw/auth"
	"github.com/versity/versitygw/backend"
	"github.com/versity/versitygw/s3api"
	"github.com/versity/versitygw/s3api/middlewares"
	"github.com/versity/versitygw/s3api/utils"
	"github.com/versity/versitygw/s3err"
)

func reject(ctx fiber.Ctx, e s3err.APIError) error {
	requestID, hostID := utils.EnsureRequestIDs(ctx)
	ctx.Status(e.StatusCode())
	ctx.Set("Content-Type", "application/xml")
	return ctx.Send(e.XMLBody(requestID, hostID))
}

// Check the original request target before router path normalization can erase
// empty or dot segments. This middleware never contacts Cloudreve.
func requestBoundary(region string) fiber.Handler {
	return func(ctx fiber.Ctx) error {
		p, e := url.PathUnescape(string(ctx.Request().URI().PathOriginal()))
		if e != nil || !strings.HasPrefix(p, "/") {
			return reject(ctx, s3err.GetAPIError(s3err.ErrInvalidURI))
		}
		if p == "/healthz" {
			return ctx.Next()
		}
		parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)
		if len(parts) == 2 && parts[1] != "" && !config.ValidKey(parts[1]) {
			return reject(ctx, s3err.GetAPIError(s3err.ErrInvalidURI))
		}
		credential := ctx.Query("X-Amz-Credential")
		if authHeader := ctx.Get("Authorization"); strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
			for _, item := range strings.Split(strings.TrimPrefix(authHeader, "AWS4-HMAC-SHA256 "), ",") {
				if strings.HasPrefix(strings.TrimSpace(item), "Credential=") {
					credential = strings.TrimPrefix(strings.TrimSpace(item), "Credential=")
				}
			}
		}
		if scope := strings.Split(credential, "/"); len(scope) == 5 && scope[2] != region {
			return reject(ctx, s3err.APIError{Code: "AuthorizationHeaderMalformed", Description: "Use the configured gateway region", HTTPStatusCode: 400})
		}
		// Reject unimplemented read subresources instead of answering with local
		// ACL metadata (which is used internally by the protocol's auth middleware).
		if ctx.Method() == "GET" || ctx.Method() == "HEAD" {
			unsupported := false
			invalidLimit := false
			ctx.Request().URI().QueryArgs().VisitAll(func(k, v []byte) {
				name := string(k)
				if strings.HasPrefix(name, "X-Amz-") || name == "x-id" {
					return
				}
				if p == "/" {
					// WinSCP's max-keys is a login probe, not ListBuckets pagination.
					// Keep it signed and return all configured buckets. Standard
					// clients page with max-buckets and continuation-token instead.
					if name == "max-keys" && ctx.Method() == "GET" {
						limit, err := strconv.ParseInt(string(v), 10, 32)
						invalidLimit = invalidLimit || err != nil || limit < 1
						return
					}
					switch name {
					case "max-buckets", "prefix", "continuation-token", "bucket-region":
						return
					}
					unsupported = true
					return
				}
				if len(parts) == 2 && parts[1] != "" {
					switch name {
					case "uploadId", "part-number-marker", "max-parts":
						return
					case "response-cache-control", "response-content-disposition", "response-content-encoding", "response-content-language", "response-content-type", "response-expires":
						return
					}
					unsupported = true
					return
				}
				switch name {
				case "uploads", "key-marker", "upload-id-marker", "max-uploads":
					return
				case "marker":
					unsupported = unsupported || ctx.Query("list-type") != ""
					return
				case "location", "list-type", "prefix", "delimiter", "max-keys", "continuation-token", "start-after", "fetch-owner", "encoding-type":
					return
				}
				unsupported = true
			})
			if invalidLimit {
				return reject(ctx, s3err.APIError{Code: "InvalidArgument", Description: "max-keys must be a positive 32-bit integer for ListBuckets", HTTPStatusCode: 400})
			}
			if unsupported {
				return reject(ctx, s3err.GetAPIError(s3err.ErrNotImplemented))
			}
		}
		return ctx.Next()
	}
}

func NewServer(be *Backend, access, secret string, options ...s3api.Option) (*s3api.S3ApiServer, error) {
	root := middlewares.RootUserConfig{Access: access, Secret: secret}
	// Versity does not expose an idle timeout. Keep its default of closing
	// connections after a response so idle SDK pools cannot occupy every slot.
	opts := []s3api.Option{s3api.WithReadOnly(), s3api.WithQuiet(), s3api.WithHealth("/healthz"), s3api.WithConcurrencyLimiter(be.Config.MaxConcurrent*2, be.Config.MaxConcurrent), s3api.WithMiddleware("/", requestBoundary(be.Config.Region))}
	if be.Config.WriteEnabled {
		opts = opts[1:] // Retain protocol authentication; omit only WithReadOnly.
	}
	opts = append(opts, options...)
	var service backend.Backend = be
	if be.Config.MultiBucket() {
		service = newMappedBackend(be)
	}
	return s3api.New(service, root, be.Config.Region, auth.NewIAMServiceSingle(auth.Account{Access: access, Secret: secret}), nil, nil, nil, nil, opts...)
}
