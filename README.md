# Silo Ebook Metadata Plugin

Standalone metadata provider plugin for
[Silo](https://github.com/Silo-Server/silo-server) ebook libraries.

## Capability

- `metadata_provider.v1`
- Capability ID: `ebook-metadata`
- Default priority: `ebook = 2`

## Sources

OpenLibrary, Google Books, ISBNdb, Hardcover, Goodreads, Amazon, Anna's Archive, Project Gutenberg, BookBrainz, FantasticFiction, ISFDB, LibraryThing, Internet Archive, WorldCat, and Douban.

Google Books, ISBNdb, and Hardcover require API keys before they make upstream requests. The other sources use public APIs or scraped catalog pages.

## Setup

Install the plugin, add **Ebook Metadata** to an ebook library's provider chain,
and configure only the sources or API keys you want to use. Sources that require
a key are skipped until one is configured.

## Identity

Ebooks use ISBN and source-specific provider IDs. The plugin maps people as authors only.

## Configuration

- `enabled_sources`: comma-separated source IDs. Empty uses the default source set.
- `google_books_api_key`: optional Google Books API key.
- `isbndb_api_key`: optional ISBNdb API key.
- `hardcover_api_key`: optional Hardcover API key.
- `default_region`: accepted as a fallback language/region hint. Current sources
  do not alter their upstream requests based on it.

## Development

Run tests:

```sh
go test ./...
```

Build the plugin binary:

```sh
make build
```

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. New
sources and matching changes should start as an issue.
