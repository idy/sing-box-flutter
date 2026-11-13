# Project website

The static source lives in `site/`. It explains selected native builds and uses the
real example profiles as its interactive YAML recipes. The website needs no Go,
Flutter, Node packages, analytics or remote browser-side services.

Build from the repository root:

```bash
python3 -B .github/scripts/build_site.py
```

The builder requires a Git checkout and writes `.build/site/`. It validates local
HTML references and reads all three example YAML files from the exact commit used
for the displayed dependency pin and source links. Downloaded YAML bytes are kept
unchanged. Commit profile changes before previewing them; uncommitted profile
edits do not alter a website pinned to an older core. All text is UTF-8 regardless
of the build machine's locale. The default profile is also embedded in the page
for readers without JavaScript. The complete idy.dev Site SDK 0.2.0 distribution
is vendored in `site/shared/sdk/`, including the self-hosted Archivo variable font,
Phosphor icons and their licenses. Load its JavaScript and CSS through relative
URLs, followed by `site/style.css`. Keep the complete SDK distribution together
when updating it. The site uses the Component Datasheet frame with a blue
masthead, white/deep-navy sheets, 2px corners and ruled specification tables.

Serve the built directory from the repository root:

```bash
PORT=4175 node .github/scripts/serve_site.mjs
```

The preview listens on localhost port 4175. Rebuild after editing the HTML, CSS,
JavaScript or profiles. Local Claude configuration under `.claude/` is ignored
by Git. Website and design working files are excluded from the pub.dev archive.

## Deployment

`.github/workflows/pages.yml` validates builds for relevant pull requests. It can
deploy only the default `main` branch to the `github-pages` environment, on every
main push or an explicit workflow dispatch. A case-sensitive runtime check
allows production only for the exact `refs/heads/main`; preview branches, including
case variants such as `Main`, cannot deploy. Only eligible deployment jobs enter
the production concurrency group, and they do not cancel an in-progress deployment.
The group retains all pending jobs. After taking the deployment lock, a job reads
the live main revision and skips an artifact built from any superseded commit.
Every main push starts a successor build, so even an unrelated main change cannot
leave the preceding website update permanently skipped.

For the initial release before the source workflow reaches main, a verified
static build can be published to the `gh-pages` branch with a CNAME file and Pages
configured to serve that branch. This does not require relaxing main's CI gates.
A repository administrator switches the Pages publishing source to Actions
separately, using an administrator-authorized login. The workflow token only
deploys content; it never updates repository Pages settings. Confirm that the
existing static release stays reachable during that one-time switch.

Before the first deployment:

1. Enable GitHub Pages for `idy/sing-box-flutter` using GitHub Actions as its source.
2. Configure the custom domain `sing-box-flutter.idy.dev` for the Pages site.
3. Set its DNS CNAME to `idy.github.io` (not a repository path or a localhost IP).
4. After DNS verification and certificate issuance, require HTTPS.
5. Deploy the reviewed `main` revision, then verify the public page, all profile
   requests and local assets over HTTPS.

Until custom DNS is configured, deploy to `https://idy.github.io/sing-box-flutter/`.
The production workflow builds an HTTPS canonical URL from the host and base path
in GitHub Pages settings, including while a new custom-domain certificate is
being provisioned. Local and PR builds use the default URL. Once DNS works, setting the custom domain in
Pages and redeploying updates the canonical and Open Graph URLs automatically.
For Actions-based deployments, configure the custom domain in Pages settings;
a CNAME file in the artifact does not create the required DNS record.

The current page offers a Git dependency pinned to the reviewed selectable-core
revision while 0.3.0 is unpublished. Once that version is available on pub.dev,
replace the source dependency example with the matching published dependency and
remove the preparation notice. Do not point visitors at 0.2.0 for selected builds.

Only after the public site is reachable, update `pubspec.yaml`'s `homepage` to
`https://sing-box-flutter.idy.dev/` and add the public link to README. pub.dev renders
the README and homepage metadata of each published package version; deploying the
website alone does not update an already-published package. Publish the prepared
0.3.0 package after its own checks and release review pass.
