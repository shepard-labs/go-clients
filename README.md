# go-clients

[![CI](https://github.com/shepard-labs/go-clients/actions/workflows/ci.yml/badge.svg)](https://github.com/shepard-labs/go-clients/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/shepard-labs/go-clients)](https://goreportcard.com/report/github.com/shepard-labs/go-clients)
[![Go Reference](https://pkg.go.dev/badge/github.com/shepard-labs/go-clients.svg)](https://pkg.go.dev/github.com/shepard-labs/go-clients)

Reusable Go clients for cloud services, extracted as independent modules so a
backend can import only what it needs.

Multi-module monorepo: each top-level directory is its own Go module with its
own dependency set. A consumer that needs only KMS pulls no email or storage
SDKs into its build graph.

## Modules

| Module | Import path | Providers |
|---|---|---|
| `email` | `github.com/shepard-labs/go-clients/email` | SES (`/ses`), Postmark (`/postmark`) |
| `storage` | `github.com/shepard-labs/go-clients/storage` | GCS (`/gcs`), Cloudflare R2 (`/r2`) |
| `kms` | `github.com/shepard-labs/go-clients/kms` | Google Cloud KMS |

Each module exposes a single capability interface; the provider subpackages
implement it and bind their credentials/configuration at construction.

## email

```go
import (
    "github.com/shepard-labs/go-clients/email"
    "github.com/shepard-labs/go-clients/email/ses"
    "github.com/shepard-labs/go-clients/email/postmark"
)

// Pick a provider; both return email.Sender.
var sender email.Sender
sender = ses.New(ses.Credentials{AccessKeyID: id, SecretAccessKey: secret, Region: "us-east-1"}, "my-service", logger)
sender = postmark.New(serverToken, logger)

res, err := sender.Send(ctx, &email.Message{
    From:     "no-reply@example.com",
    To:       []string{"user@example.com"},
    Subject:  "Hello",
    HTMLBody: "<p>Hi</p>",
})
err = sender.VerifyAuth(ctx)
```

The `Sender` interface covers `Send` and `VerifyAuth`. Provider-specific
operations (SES identity management, Postmark domain verification, account /
server introspection) are methods on the concrete `*ses.Client` /
`*postmark.Client` and reachable via a type assertion.

## storage

```go
import (
    "github.com/shepard-labs/go-clients/storage"
    "github.com/shepard-labs/go-clients/storage/gcs"
    "github.com/shepard-labs/go-clients/storage/r2"
)

var s storage.Storage
s, err = gcs.New(ctx, serviceAccountB64, "my-bucket", "my-service", logger)
s, err = r2.New(accountID, accessKey, secret, "my-bucket", "my-service", logger)

err = s.Upload(ctx, "path/to/object", content, "application/pdf")
err = s.UploadReader(ctx, "path/to/object", reader, "application/pdf", size)
data, err := s.Download(ctx, "path/to/object")
err = s.Delete(ctx, "path/to/object")
err = s.Close()
```

The bucket is supplied at construction. Application-specific path conventions
are intentionally out of scope — build them on top of a `storage.Storage`
value.

## kms

```go
import "github.com/shepard-labs/go-clients/kms"

enc, err := kms.New(ctx, serviceAccountB64,
    "projects/p/locations/global/keyRings/r/cryptoKeys/k", logger)

ciphertext, err := enc.EncryptCredentials(ctx, plaintext)
plaintext, err := enc.DecryptCredentials(ctx, ciphertext)
err = enc.Close()
```

The full KMS crypto-key resource name is supplied at construction.
