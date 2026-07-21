# Silo Ebook Metadata Plugin

Standalone metadata provider plugin for Silo ebook libraries.

## Capability

- `metadata_provider.v1`
- Capability ID: `ebook-metadata`
- Default priority: `ebook = 2`

## Sources

Sources are split into three operational tiers:

- **Identifier:** OpenLibrary, Google Books, and ISBNdb. ISBN lookups call
  these sources sequentially in that order and stop after a complete exact
  match.
- **Catalog:** OpenLibrary, Google Books, ISBNdb, and Hardcover. These general
  sources are enabled by default and title/author searches query them with
  bounded concurrency.
- **Extended/specialized:** Project Gutenberg, BookBrainz, Internet Archive,
  Goodreads, Amazon, Anna's Archive, FantasticFiction, ISFDB, LibraryThing,
  WorldCat, and Douban. These sources are disabled by default. When explicitly
  enabled, they are queried only if the catalog tier has no high-confidence
  match.

Google Books, ISBNdb, and Hardcover require API keys before they make upstream
requests. They remain in the default catalog set but return no results until
their corresponding key is configured.

## Matching

Search results are ranked deterministically. An exact normalized ISBN ranks
first, followed by normalized title plus author, then title plus publication
year or language. Results are deduplicated by normalized ISBN, or by source and
provider ID when no ISBN is available. Ambiguous title-only collisions are
discarded.

Ebooks use ISBN and source-specific provider IDs. The plugin maps people as
authors only.

## Source Health

Every source has an independent in-memory cooldown. HTTP 403 and 405 responses
open the source circuit for one hour. HTTP 429 honors `Retry-After` delta
seconds and HTTP dates, with a five-minute fallback when the header is absent
or invalid. Timeouts and other transient failures start at 30 seconds and back
off exponentially to one hour. Any healthy response, including a successful
no-match response, closes the circuit. A source in cooldown is skipped while
healthy sources continue to serve the request. When no eligible source
completes successfully, the request returns a source availability error rather
than a false no-match.

Open Library requests are admitted at 60 requests per minute because the
plugin's current user agent does not include contact identification. Waiting
for admission respects request cancellation and deadlines.

Each source also has one shared concurrency budget across all active ebook
jobs. Saturated sources defer excess work immediately instead of allowing a
large Silo worker pool to create an upstream request flood.

Cooldown state is process-local and resets when the plugin restarts.

## Configuration

- `enabled_sources`: comma-separated source IDs. Empty enables the reliable
  catalog set above. A non-empty value is an exact override: only explicitly
  listed sources are enabled, including extended sources, and their listed
  order defines lookup priority.
- `google_books_api_key`: optional Google Books API key.
- `isbndb_api_key`: optional ISBNdb API key.
- `hardcover_api_key`: optional Hardcover API key.
- `default_region`: optional region hint used by regional sources.

## Development

Run tests:

```sh
go test ./...
```

Build the plugin binary:

```sh
make build
```
