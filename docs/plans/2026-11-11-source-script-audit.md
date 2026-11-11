# Source and deployment audit repair

Goal: measurements describe received data, management commands affect the named live node,
cleanup never trusts recycled PIDs, and every supported server installation uses working,
consistent credentials, engine configs and keeper protocols.

1. Accurate measurement: reject HTTP errors, truncated bodies and timeouts; regression tests
   must fail first, then pass for real payloads. Bound gain requests and verify application.
2. Safe control: preserve omitted gain; reject tuning a node other than the running daemon's
   node; honor configured governor ceilings; validate sing-box identity before signaling.
   Behavioral tests cover these safety and quality rules before implementation.
3. Consistent deployment: share one embedded/standalone keeper; generate engine configs and
   client URIs from one persisted identity; use independent TCP/UDP services, verified engine
   downloads, private credentials, fail-fast bootstrap and proper container shutdown.
   Test real generated configs with the installed engine and exercise local keeper HTTP.
4. Usable installation: replace the dangling installer redirect with a real standalone
   installer, generate cloud-init from the same assets, update documented import commands.
   Verify generated artifacts, shell syntax and sandboxed bootstrap behavior.

Final gates: scripts/verify-performance.sh (race tests, Python tests, build), go vet,
shell syntax, regenerated cloud-init equality, diff whitespace and staged-secret review.
No live server, Docker daemon, host network settings or remote resources will be mutated.
Changes are committed on a persistent repository branch. No push/deployment is requested.
