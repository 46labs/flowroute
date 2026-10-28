# flowroute

Mock of the Flowroute Messaging API for local development and tests.

## Run

```bash
just run       # :8080, reads ./config.yaml if present
just docker    # same, in a container, with config.example.yaml mounted
just ci        # gofmt, vet, race tests, golangci-lint
```

See `config.example.yaml`. Scalar keys can be set as upper-case environment
variables (`ACCESS_KEY`, `CALLBACK`, `DLR_DELAY`); `numbers` and `outcomes`
come from the config file only.

## Flowroute API

| Request | Behaviour |
|---|---|
| `POST /v2.1/messages`, `POST /v2.2/messages` | Basic auth (`access_key:secret_key`), `Content-Type: application/vnd.api+json`. Takes the JSON:API document or the compact form (top-level `to`, `from`, `body`). `from` must be a configured number. Returns `202` with `mdr2-` ID; v2.2 adds `price_details`. |
| `GET /v2.1/messages/{id}`, `GET /v2.2/messages/{id}` | MDR lookup, receipts newest first. |

Errors: `401` bad credentials, `415` wrong content type, `403` `from` not on
the account, `400` invalid document, `429` over `sms_per_second` or
`mms_per_second`.

After a send, the mock records a level 1 receipt (`smsc submit`) and then the
level 2 receipt for the destination's outcome, `dlr_delay` apart, and posts
each to the message's `dlr_callback` or the account `dlr_callback`:

| Outcome | Level 2 status |
|---|---|
| `delivered` (default) | `delivered` |
| `buffered` | `message buffered`, code `1003` |
| `undelivered` | `undelivered` |
| `none` | no level 2 receipt |

Callbacks are retried until a `200`, `attempts` times, `retry_interval`
apart (Flowroute: 4 attempts, 30 s).

## Control API

| Request | Behaviour |
|---|---|
| `POST /mock/inbound` | `{"from","to","body","media":[{"url","mime_type","file_size","file_name"}]}`. Posts a Flowroute inbound message to the number's callback, or the account callback. Returns `202` `{"id"}`. |
| `GET /mock/messages` | Every message with its receipts and callback attempts. |
| `GET /mock/messages/{id}` | One message. |
| `DELETE /mock/messages` | Clear messages. |
| `GET /healthz` | `200`. |

## Differences from Flowroute

- Status codes and descriptions for `delivered` and `undelivered` are not
  documented by Flowroute; the mock sends `status_code: null` and its own
  descriptions.
- `message_encoding` is `0` for GSM-7 and `8` for UCS-2 (SMPP `data_coding`);
  Flowroute documents only `0`.
- The `403`, `400` and `429` bodies follow the JSON:API error shape of the
  documented `401` and `403`; their text is the mock's.
