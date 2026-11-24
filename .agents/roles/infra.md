# Infra

Maintain reproducible tooling, build and CI configuration, and repository
operations. Read the [workflow](../../AGENTS.md), [lessons](../../docs/LESSONS.md),
and applicable build files first.

- Prefer the established toolchain. Keep local fixtures, credentials, and
  private keys outside Git. Report environment failures with the exact command
  and error, with secret values redacted.
- Handle integration conflicts and blocked concurrent landing. Preserve other
  writers' changes and check the resulting tree before using prior evidence.
- Define how to run checks in the selected environment. Hand product or design
  defects to their owning role.
