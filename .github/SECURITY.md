# Biralobase security

Biralobase is an independent Biralo Studio project based on PocketBase and Turso. Report issues in this fork to the Biralo Studio maintainers of this repository.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting when it is available. Otherwise, open a repository issue requesting a private reporting channel, without including exploit details, credentials or customer data. Send the details privately once a maintainer supplies that channel.

Include the Biralobase commit or image digest, deployment mode, affected component, minimal reproduction, expected and actual behavior, and the permissions required to trigger the issue. Use synthetic data and remove secrets from logs and backups.

## Release status and trust boundaries

Biralobase is experimental. The current qualification results and unresolved release blockers are in [the second review](../docs/second-review.md) and [the testing policy](../docs/testing.md).

Application functions and inherited server hooks are trusted code. Superuser access, deployment credentials and the ability to change application bundles grant administrative capabilities. OAuth and MCP tools must enforce the application user's collection, row and field permissions.

PocketBase and Turso retain their own upstream security policies. The Biralobase maintainers can coordinate upstream reports when a reproduced issue affects those projects.
