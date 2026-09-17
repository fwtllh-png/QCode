import pathlib
import shutil
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]


class CheckDocsTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        subprocess.run(["git", "init", "-q"], cwd=self.root, check=True)
        for name in (
            "README.md", "CONTRIBUTING.md", "SECURITY.md",
            "scripts/README.md", "docs/zh-CN/README.md",
        ):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("# 文档\n", encoding="utf-8")
        shutil.copyfile(
            ROOT / "scripts/check-docs.sh",
            self.root / "scripts/check-docs.sh",
        )

    def run_check(self):
        return subprocess.run(
            ["bash", str(self.root / "scripts/check-docs.sh")],
            cwd=self.root, capture_output=True, text=True,
        )

    def test_product_documentation_passes_without_book(self):
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("5 Markdown files", result.stdout)

    def test_missing_and_escaping_links_still_fail(self):
        for link in ("missing.md", "../outside.md"):
            with self.subTest(link=link):
                (self.root / "README.md").write_text(
                    f"[目标]({link})\n", encoding="utf-8",
                )
                result = self.run_check()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("README.md:", result.stderr)

    def test_required_entry_and_english_tree_still_fail(self):
        (self.root / "SECURITY.md").unlink()
        (self.root / "docs/en").mkdir()
        result = self.run_check()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing Chinese documentation entry: SECURITY.md", result.stderr)
        self.assertIn("English documentation tree must not exist", result.stderr)

    def test_deleted_document_reference_fails(self):
        path = self.root / "docs/zh-CN/deleted.md"
        path.write_text("# 旧文档\n", encoding="utf-8")
        subprocess.run(["git", "add", "."], cwd=self.root, check=True)
        path.unlink()
        (self.root / "README.md").write_text(
            "[旧文档](docs/zh-CN/deleted.md)\n", encoding="utf-8",
        )
        result = self.run_check()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing link target", result.stderr)


if __name__ == "__main__":
    unittest.main()
