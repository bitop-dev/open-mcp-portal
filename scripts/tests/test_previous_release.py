"""Tests of scripts/previous-release.sh against a throwaway git repository and a
stub registry on a loopback port; no network, no cluster.

Run: python3 -m unittest discover -s scripts/tests -p 'test_previous_release.py' -v
"""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

SCRIPT = Path(__file__).resolve().parents[1] / "previous-release.sh"
# Releases up to v0.15.2 (the script's default boundary) are published under
# the legacy repository, later ones under the current one. The stub registry
# serves each tag only from the repository it was published to.
LEGACY = "ncecere/open-mcp-portal"
CURRENT = "bitop-dev/open-mcp-portal"
REPO = LEGACY
DIGESTS = {
    (LEGACY, "v0.7.0"): "sha256:" + "a" * 64,
    (LEGACY, "v0.8.0"): "sha256:" + "b" * 64,
    (LEGACY, "v0.15.2"): "sha256:" + "d" * 64,
    (CURRENT, "v0.15.3"): "sha256:" + "e" * 64,
    (CURRENT, "v0.15.10"): "sha256:" + "f" * 64,
}
SCOPES = []


class Registry(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/token"):
            SCOPES.append(self.path)
            body = b'{"token":"stub-token"}'
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.end_headers()

    def do_HEAD(self):
        repo, sep, tag = self.path[len("/v2/"):].partition("/manifests/") if self.path.startswith("/v2/") else ("", "", "")
        key = (repo, tag)
        if self.headers.get("Authorization") != "Bearer stub-token" or key not in DIGESTS:
            self.send_response(401 if key in DIGESTS else 404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Docker-Content-Digest", DIGESTS[key])
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args):
        pass


def git(cwd, *args):
    subprocess.run(
        ["git", "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false",
         "-c", "tag.gpgsign=false", *args],
        cwd=cwd, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


class PreviousReleaseTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), Registry)
        cls.url = "http://127.0.0.1:%d" % cls.server.server_address[1]
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="omp-prev-test-")
        self.addCleanup(self.tmp.cleanup)
        self.repo = Path(self.tmp.name)
        (self.repo / "scripts").mkdir()
        shutil.copy(SCRIPT, self.repo / "scripts" / "previous-release.sh")
        git(self.repo, "init", "-q", "-b", "main")
        self.commit("one")

    def commit(self, name):
        (self.repo / name).write_text(name)
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", name)

    def pin_example(self, tag, digest, name="ghcr.io/" + LEGACY, new_name=None):
        d = self.repo / "deploy" / "kubernetes" / "overlays" / "example"
        d.mkdir(parents=True, exist_ok=True)
        entry = "  - name: %s\n" % name
        if new_name:
            entry += "    newName: %s\n" % new_name
        entry += "    newTag: %s\n    digest: %s\n" % (tag, digest)
        (d / "kustomization.yaml").write_text(
            "resources:\n  - ../../base\nimages:\n" + entry
            + "configMapGenerator:\n  - name: open-mcp-portal-config\n    behavior: merge\n")

    def run_script(self, *args):
        return self.run_script_env({}, *args)

    def test_newest_final_tag_reachable_from_head(self):
        git(self.repo, "tag", "v0.7.0")
        self.commit("two")
        git(self.repo, "tag", "v0.8.0")
        self.commit("three")
        git(self.repo, "tag", "v0.9.0-rc.1")  # a pre-release is not "the previous release"
        self.commit("four")
        r = self.run_script("--tag-only")
        self.assertEqual((r.returncode, r.stdout.strip()), (0, "v0.8.0"), r.stderr)

    def test_a_release_tag_at_head_is_skipped(self):
        git(self.repo, "tag", "v0.7.0")
        self.commit("two")
        git(self.repo, "tag", "v0.8.0")
        r = self.run_script("--tag-only")
        self.assertEqual((r.returncode, r.stdout.strip()), (0, "v0.7.0"), r.stderr)

    def test_tags_not_reachable_from_head_are_ignored(self):
        git(self.repo, "tag", "v0.7.0")
        git(self.repo, "checkout", "-q", "-b", "side")
        self.commit("side")
        git(self.repo, "tag", "v0.9.0")
        git(self.repo, "checkout", "-q", "main")
        self.commit("two")
        r = self.run_script("--tag-only")
        self.assertEqual((r.returncode, r.stdout.strip()), (0, "v0.7.0"), r.stderr)

    def test_unpublished_tags_are_skipped(self):
        git(self.repo, "tag", "v0.14.0")
        self.commit("two")
        git(self.repo, "tag", "v0.15.0")  # tagged, never published
        self.commit("three")
        r = self.run_script("--tag-only")
        self.assertEqual((r.returncode, r.stdout.strip()), (0, "v0.14.0"), r.stderr)
        r = self.run_script_env({"OMP_UNPUBLISHED_TAGS": ""}, "--tag-only")
        self.assertEqual((r.returncode, r.stdout.strip()), (0, "v0.15.0"), r.stderr)

    def run_script_env(self, extra, *args):
        env = dict(os.environ, OMP_REGISTRY_URL=self.url, NO_PROXY="127.0.0.1", no_proxy="127.0.0.1")
        for name in ("OMP_IMAGE_REPO", "OMP_LEGACY_IMAGE_REPO", "OMP_LEGACY_IMAGE_LAST", "OMP_TOKEN_URL"):
            env.pop(name, None)  # the script's defaults are under test
        env.update(extra)
        return subprocess.run(["bash", str(self.repo / "scripts" / "previous-release.sh"), *args],
                              env=env, capture_output=True, text=True, timeout=30)

    def test_versions_sort_numerically(self):
        for tag in ("v0.9.0", "v0.10.0", "v0.2.1"):
            self.commit("c" + tag)
            git(self.repo, "tag", tag)
        self.commit("later")
        r = self.run_script("--tag-only")
        self.assertEqual(r.stdout.strip(), "v0.10.0", r.stderr)

    def test_no_tag_fails_with_a_hint(self):
        r = self.run_script("--tag-only")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("fetch tags", r.stderr)

    def test_prints_tag_and_digest_reference(self):
        git(self.repo, "tag", "v0.8.0")
        self.commit("two")
        r = self.run_script()
        host = self.url.split("://", 1)[1]
        self.assertEqual((r.returncode, r.stdout.strip()),
                         (0, "v0.8.0 %s/%s@%s" % (host, REPO, DIGESTS[(REPO, "v0.8.0")])), r.stderr)

    def test_explicit_tag(self):
        r = self.run_script("--tag", "v0.7.0")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(r.stdout.strip().endswith("@" + DIGESTS[(LEGACY, "v0.7.0")]))

    def resolve(self, tag):
        SCOPES.clear()
        r = self.run_script("--tag", tag)
        host = self.url.split("://", 1)[1]
        return r, host

    def test_v0_15_2_and_earlier_are_read_from_the_legacy_repository(self):
        for tag in ("v0.7.0", "v0.15.2"):
            r, host = self.resolve(tag)
            self.assertEqual((r.returncode, r.stdout.strip()),
                             (0, "%s %s/%s@%s" % (tag, host, LEGACY, DIGESTS[(LEGACY, tag)])), r.stderr)
            self.assertTrue(all("repository:%s:pull" % LEGACY in s for s in SCOPES), SCOPES)

    def test_later_releases_are_read_from_the_current_repository(self):
        # v0.15.10 sorts after v0.15.2 as a version, not as a string.
        for tag in ("v0.15.3", "v0.15.10"):
            r, host = self.resolve(tag)
            self.assertEqual((r.returncode, r.stdout.strip()),
                             (0, "%s %s/%s@%s" % (tag, host, CURRENT, DIGESTS[(CURRENT, tag)])), r.stderr)
            self.assertTrue(all("repository:%s:pull" % CURRENT in s for s in SCOPES), SCOPES)

    def test_the_previous_release_across_the_move_is_the_legacy_image(self):
        # On the v0.15.3 tag the upgrade gate starts from v0.15.2, in the old
        # repository; on a later tag it starts from v0.15.3, in the new one.
        git(self.repo, "tag", "v0.15.2")
        self.commit("two")
        self.pin_example("v0.15.3", "sha256:" + "0" * 64, name="ghcr.io/" + CURRENT)
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", "release")
        git(self.repo, "tag", "v0.15.3")
        host = self.url.split("://", 1)[1]
        r = self.run_script()
        self.assertEqual((r.returncode, r.stdout.strip()),
                         (0, "v0.15.2 %s/%s@%s" % (host, LEGACY, DIGESTS[(LEGACY, "v0.15.2")])), r.stderr)
        self.commit("three")
        r = self.run_script()
        self.assertEqual(r.returncode, 1, r.stdout)  # the placeholder pin for v0.15.3 is compared now
        self.assertIn("the tag moved, or the pin is wrong", r.stderr)
        self.pin_example("v0.15.3", DIGESTS[(CURRENT, "v0.15.3")], name="ghcr.io/" + CURRENT)
        r = self.run_script()
        self.assertEqual((r.returncode, r.stdout.strip()),
                         (0, "v0.15.3 %s/%s@%s" % (host, CURRENT, DIGESTS[(CURRENT, "v0.15.3")])), r.stderr)

    def test_the_boundary_and_repositories_can_be_overridden(self):
        r = self.run_script_env({"OMP_LEGACY_IMAGE_LAST": "v0.7.0"}, "--tag", "v0.8.0")
        self.assertNotEqual(r.returncode, 0)  # v0.8.0 is not in the current repository
        self.assertIn("could not read the manifest of %s:v0.8.0" % CURRENT, r.stderr)
        r = self.run_script_env({"OMP_IMAGE_REPO": LEGACY, "OMP_LEGACY_IMAGE_LAST": "v0.7.0"}, "--tag", "v0.8.0")
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_a_tag_that_is_not_a_release_is_refused(self):
        r = self.run_script("--tag", "main")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("not a release tag", r.stderr)

    def test_unknown_tag_on_the_registry_fails(self):
        r = self.run_script("--tag", "v9.9.9")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("could not read the manifest", r.stderr)

    def test_example_overlay_pin_must_agree_with_the_registry(self):
        git(self.repo, "tag", "v0.8.0")
        self.commit("two")
        self.pin_example("v0.8.0", DIGESTS[(LEGACY, "v0.8.0")])
        self.assertEqual(self.run_script().returncode, 0)
        self.pin_example("v0.8.0", "sha256:" + "c" * 64)
        r = self.run_script()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("the tag moved, or the pin is wrong", r.stderr)

    def test_example_overlay_pin_must_name_the_release_s_repository(self):
        git(self.repo, "tag", "v0.15.2")
        self.commit("two")
        # The base's name (the kustomize match key) without newName renders the
        # current repository, where v0.15.2 does not exist.
        self.pin_example("v0.15.2", DIGESTS[(LEGACY, "v0.15.2")], name="ghcr.io/" + CURRENT)
        r = self.run_script()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("but v0.15.2 is published as %s" % LEGACY, r.stderr)
        self.pin_example("v0.15.2", DIGESTS[(LEGACY, "v0.15.2")], name="ghcr.io/" + CURRENT,
                         new_name="ghcr.io/" + LEGACY)
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.pin_example("v0.15.2", DIGESTS[(LEGACY, "v0.15.2")], name="ghcr.io/" + LEGACY,
                         new_name='"ghcr.io/%s"' % CURRENT)
        r = self.run_script()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("pins v0.15.2 as ghcr.io/%s," % CURRENT, r.stderr)

    def test_a_pin_for_another_tag_is_not_compared(self):
        git(self.repo, "tag", "v0.7.0")
        self.commit("two")
        self.pin_example("v0.8.0", "sha256:" + "c" * 64)
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stderr)


if __name__ == "__main__":
    unittest.main()
