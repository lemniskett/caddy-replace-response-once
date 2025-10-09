Caddy `replace_response_once` handler module
=======================================

A fork of the [replace_response](https://github.com/caddyserver/replace-response) module, with modifications to only allow single-instance replacements (i.e., replacing only the first occurrence of a match).

Note: Does not support regex-based replacements.

**Module name:** `http.handlers.replace_response`


## JSON examples

Substring substitution:

```json
{
	"handler": "replace_response_once",
	"replacements": [
		{
			"search": "Foo",
			"replace": "Bar"
		}
	]
}
```

Same, but with streaming mode (we just set `"stream": true` in the handler):

```json
{
	"handler": "replace_response_once",
	"replacements": [
		{
			"search": "Foo",
			"replace": "Bar"
		}
	],
	"stream": true
```

With a response matcher:
```json
{
	"handler": "replace_response_once",
	"replacements": [
		{
			"search": "Foo",
			"replace": "Bar"
		}
	],
	"match": {
		"headers": {
			"Content-Type": ["application/json*"]
		}
	}
}
```

## Caddyfile

This module has Caddyfile support. It registers the `replace` directive, by default to be after the standard `encode` directive. Make sure to change it with the [order](https://caddyserver.com/docs/caddyfile/directives#directive-order) global option in case that is not suitable for your needs.

Syntax:

```
replace [<matcher>] [stream | <search> <replace>] {
	stream
	match {
		header Content-Type application/json*
	}
	[re] <search> <replace>
}
```

- `stream` enables streaming mode.
- `match` defines a [response matcher](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#response-matcher). If defined, replacements in this directive will only be performed on responses that match the matcher.
- Note that you can use a matcher token to filter which requests have replacements performed.

Simple substring substitution:

```
replace Foo Bar
```

Streaming mode:

```
replace stream {
	Foo Bar
}
```

Multiple replacements:

```
replace {
	Foo Bar
	A B
}
```

## Limitations:

- Compressed responses (e.g. from an upstream proxy which gzipped the response body) will not be decoded before attempting to replace. To work around this, you may send the `Accept-Encoding: identity` request header to the upstream to tell it not to compress the response. For example:

      reverse_proxy localhost:8080 {
          header_up Accept-Encoding identity
      }
