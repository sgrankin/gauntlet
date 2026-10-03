# Maintaining the documentation

Markdown in `docs/` is the source for both GitHub browsing and the MkDocs
Material site. Keep local links relative so both views work.

## Build and preview

```sh
python3 -m venv .venv-docs
.venv-docs/bin/pip install -r requirements-docs.txt
make docs-check
make docs-serve
```

The preview serves at `http://127.0.0.1:8000`. Builds go to `site/`, which is
ignored by Git. CI builds in strict mode and checks local links and anchors.
Python is needed only for documentation tooling, not for the daemon.

## Add or edit a page

1. Choose its owner: task guides, references, operations, runbooks, or architecture.
2. Keep the directory tree two levels deep and add the page to `mkdocs.yml` navigation.
   Place it after its prerequisites and beside related tasks. Navigation follows
   the reader's workflow; do not sort it by filename or title.
3. State a contract in one place, linking from examples and rationale.
4. Use tables for settings/defaults, numbered procedures for tasks, and short
   code examples. Explain surprising choices outside the code rather than
   narrating each line with comments.
5. Verify examples against current config parsing and run `make docs-check`.

Keep README and DESIGN as entry points. Git history records completed work;
document current behavior and material limitations rather than implementation diaries.

## GitHub Pages

The repository includes a manually dispatched Pages publication workflow.
To launch the site:

1. In repository **Settings → Pages**, choose **GitHub Actions** as the source.
2. Run the **Documentation** workflow on `main`, setting **Publish to GitHub Pages**.
3. After a successful deployment, open `https://sgrankin.github.io/gauntlet/`.

Pushes and pull requests build and validate without publishing. Preview artifacts
are available from the workflow's build job. Enable automatic deployment only
when the repository owner chooses it; the deployment job already has the required
Pages and OIDC permissions.
