import importlib.util
import pathlib
import unittest

_spec = importlib.util.spec_from_file_location("check_no_make", pathlib.Path(__file__).with_name("check-no-make.py"))
cnm = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(cnm)


def hits(text):
    return cnm.scan_text("x.md", text)


class InstructionTests(unittest.TestCase):
    def test_flags_the_old_instruction_shapes(self):
        for line in [
            "run `make deploy` to install",
            "make deploy-platform",
            "$ make set-anthropic-key MODELS='claude-*'",
            "make -n help > /dev/null",
            "make dev-up DEV_PORT=5174",
            "make deploy CLOUD=aws",
            "the $(MAKE) cli race",
            "use `make` here",
            "make destroy",
            "make test",
        ]:
            self.assertTrue(hits(line), line)

    def test_ignores_english(self):
        for line in [
            "make sure the cluster is up",
            "make it work",
            "so a stale token cannot make a write lose",
            "does not make sense",
            "make no changes",
            "the Makefile flow is gone",
            "a Makefile-era install",
            "make tests pass",
            "makes deploy faster",
            "scripts/dev.sh up",
        ]:
            self.assertFalse(hits(line), line)

    def test_filenames(self):
        for name in ["Makefile", "darkplum/Makefile", "x/GNUmakefile", "rules.mk", "makefile"]:
            self.assertTrue(cnm.FILENAME.search(name), name)
        for name in ["scripts/dev.sh", "docs/makefile-notes.md", "darkplum/services/cli/cmd/main.go"]:
            self.assertFalse(cnm.FILENAME.search(name), name)

    def test_history_is_allowlisted_explicitly(self):
        self.assertTrue(cnm.allowed("darkplum/services/api/internal/db/migrations/017_vendor_provider_settings.sql"))
        self.assertTrue(cnm.allowed("darkplum/services/cli/internal/semrouter/testdata/values-8907e6d.yaml"))
        self.assertFalse(cnm.allowed("darkplum/README.md"))
        self.assertFalse(cnm.allowed("darkplum/services/web/src/lib/modelRouting.ts"))


if __name__ == "__main__":
    unittest.main()
