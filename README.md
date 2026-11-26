Caddy `replace_response_once` handler module
=======================================

A fork of the [replace_response](https://github.com/caddyserver/replace-response) module that replaces only the first occurrence of each search string. Later occurrences are left as they are. It does substring replacement only, not regular expressions.

It installs alongside the original module: the module, handler and directive names are all `replace_response_once`.

**Module name:** `http.handlers.replace_response_once`


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
}
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

This module has Caddyfile support. It registers the `replace_response_once` directive, by default to be after the standard `encode` directive. Make sure to change it with the [order](https://caddyserver.com/docs/caddyfile/directives#directive-order) global option in case that is not suitable for your needs.

Syntax:

```
replace_response_once [<matcher>] [stream | <search> <replace>] {
	stream
	match {
		header Content-Type application/json*
	}
	<search> <replace>
}
```

- `stream` enables streaming mode.
- `match` defines a [response matcher](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#response-matcher). If defined, replacements in this directive will only be performed on responses that match the matcher.
- Note that you can use a matcher token to filter which requests have replacements performed.

Simple substring substitution:

```
replace_response_once Foo Bar
```

Streaming mode:

```
replace_response_once stream {
	Foo Bar
}
```

Multiple replacements:

```
replace_response_once {
	Foo Bar
	A B
}
```

## Limitations:

- A search string can be at most 2048 bytes. Until its first match, the handler holds back up to that many bytes between writes, so a match split across writes is still found.

- Each search string is replaced once per response. With several replacements, they apply in order, and each one sees the output of the one before.

- Compressed responses (e.g. from an upstream proxy which gzipped the response body) will not be decoded before attempting to replace. To work around this, you may send the `Accept-Encoding: identity` request header to the upstream to tell it not to compress the response. For example:

      reverse_proxy localhost:8080 {
          header_up Accept-Encoding identity
      }
