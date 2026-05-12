# inb — the agent inbox

A tiny Go CLI for IMAP and SMTP. No SDK, no OAuth dance, no TTY required.
Designed for AI agents and scripts: JSON output, env-based auth, idempotent
commands, predictable exit codes.

## Install

    go install github.com/yuzu-hub/inb@latest

## Setup

    inb setup    # interactive: detect provider, test creds, write ~/.inb.env

## Use

    inb list --unread --limit 5
    inb read 12345
    inb send --to alice@example.com --subject "hi" --body "hello"
    inb search "invoice"
    inb folders --json

## Docs

- LLM-friendly reference: [`docs/llms.txt`](docs/llms.txt)
- Project page: [`docs/index.html`](docs/index.html)

## License

[MIT](LICENSE)
