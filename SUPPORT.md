# Getting support

MANVI is maintained by one person. This page says where each kind of question
goes so that it reaches somebody who can answer it.

| You want to… | Go to |
|---|---|
| Ask how something works, or how to set it up | [GitHub Discussions](https://github.com/bharathvbcr/Manvi/discussions) — Q&A |
| Float an idea that is not yet a scoped request | [GitHub Discussions](https://github.com/bharathvbcr/Manvi/discussions) — Ideas |
| Report a defect | [Bug report](https://github.com/bharathvbcr/Manvi/issues/new?template=bug_report.yml) |
| Propose a specific, scoped change | [Feature request](https://github.com/bharathvbcr/Manvi/issues/new?template=feature_request.yml) |
| Report a vulnerability | [Private vulnerability report](https://github.com/bharathvbcr/Manvi/security/advisories/new), as described in [SECURITY.md](SECURITY.md) |
| Report a Code of Conduct problem | [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) |
| Contribute a change | [CONTRIBUTING.md](CONTRIBUTING.md) |

## Before you ask

- Run `manvi doctor`. It prints the posture, which binary each DevCouncil
  component resolved to, and whether each one answered. Most "it does not work on
  my machine" reports are an installed component that is older than the contract
  the harness expects, and `doctor` is where that shows up.
- Search [existing issues](https://github.com/bharathvbcr/Manvi/issues?q=is%3Aissue)
  and discussions.
- The [documentation](docs/README.md) covers the CLI, configuration, the policy
  ladder and local models in detail.

## What to include in a bug report

`manvi --version`, your operating system, the model and provider you were using,
and the output of `manvi doctor`. For a policy or gate question, the decision
payload from the run report and the session log are usually the shortest path to
an answer. Remove anything you would not want public before pasting it.

## Expectations

There is no support contract and no response-time guarantee. This is a personal
project, and a reply will be as fast as one person can make it. Security reports
are read first.

Do not report vulnerabilities in a public issue or a discussion.
