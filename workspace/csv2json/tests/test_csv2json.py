"""Tests for csv2json. Run: python -m unittest discover csv2json/tests"""
import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(os.path.dirname(HERE), "csv2json.py")
sys.path.insert(0, os.path.dirname(HERE))
import csv2json  # noqa: E402


class Base(unittest.TestCase):
    def setUp(self):
        self._td = tempfile.TemporaryDirectory()
        self.addCleanup(self._td.cleanup)
        self.dir = self._td.name

    def write(self, name, data):
        path = os.path.join(self.dir, name)
        with open(path, "wb") as f:
            f.write(data if isinstance(data, bytes) else data.encode("utf-8"))
        return path

    def run_main(self, argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = csv2json.main(argv)
        return code, out.getvalue(), err.getvalue()


class TestConvert(Base):
    def test_basic(self):
        p = self.write("a.csv", "name,age\nAlice,30\nBob,25\n")
        code, out, _ = self.run_main([p])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out), [{"name": "Alice", "age": "30"}, {"name": "Bob", "age": "25"}])

    def test_output_file(self):
        p = self.write("a.csv", "x,y\n1,2\n")
        o = os.path.join(self.dir, "out.json")
        code, out, _ = self.run_main([p, "-o", o])
        self.assertEqual(code, 0)
        self.assertEqual(out, "")
        with open(o, encoding="utf-8") as f:
            self.assertEqual(json.load(f), [{"x": "1", "y": "2"}])

    def test_indent(self):
        p = self.write("a.csv", "x\n1\n")
        _, out4, _ = self.run_main([p, "--indent", "4"])
        self.assertIn('\n    {', out4)
        _, out0, _ = self.run_main([p, "--indent", "0"])
        self.assertEqual(out0.strip(), '[{"x": "1"}]')

    def test_delimiter(self):
        p = self.write("a.csv", "a;b\n1;2\n")
        _, out, _ = self.run_main([p, "--delimiter", ";"])
        self.assertEqual(json.loads(out), [{"a": "1", "b": "2"}])

    def test_bad_delimiter(self):
        p = self.write("a.csv", "a\n1\n")
        code, _, err = self.run_main([p, "--delimiter", ";;"])
        self.assertNotEqual(code, 0)
        self.assertIn("error", err)

    def test_bom(self):
        p = self.write("a.csv", b"\xef\xbb\xbfname,age\nAlice,30\n")
        _, out, _ = self.run_main([p])
        self.assertEqual(json.loads(out), [{"name": "Alice", "age": "30"}])

    def test_quotes_and_commas(self):
        p = self.write("a.csv", 'name,note\n"Smith, John","say ""hi"""\n')
        _, out, _ = self.run_main([p])
        self.assertEqual(json.loads(out), [{"name": "Smith, John", "note": 'say "hi"'}])

    def test_unicode(self):
        p = self.write("a.csv", "名字\n张三\n")
        _, out, _ = self.run_main([p])
        self.assertIn("张三", out)

    def test_empty_file(self):
        p = self.write("e.csv", b"")
        code, out, _ = self.run_main([p])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out), [])

    def test_header_only(self):
        p = self.write("h.csv", "a,b\n")
        code, out, _ = self.run_main([p])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out), [])

    def test_missing_file(self):
        code, out, err = self.run_main([os.path.join(self.dir, "nope.csv")])
        self.assertNotEqual(code, 0)
        self.assertIn("error", err)
        self.assertEqual(out, "")


class TestCLI(Base):
    def run_cli(self, args, stdin=b""):
        return subprocess.run([sys.executable, SCRIPT] + args, input=stdin,
                              capture_output=True, timeout=30)

    def test_stdin(self):
        r = self.run_cli(["-"], b"a,b\n1,2\n")
        self.assertEqual(r.returncode, 0)
        self.assertEqual(json.loads(r.stdout), [{"a": "1", "b": "2"}])

    def test_stdin_bom_delimiter(self):
        r = self.run_cli(["-", "--delimiter", ";"], b"\xef\xbb\xbfa;b\n1;2\n")
        self.assertEqual(json.loads(r.stdout), [{"a": "1", "b": "2"}])

    def test_missing_file_exit_code(self):
        r = self.run_cli([os.path.join(self.dir, "nope.csv")])
        self.assertNotEqual(r.returncode, 0)
        self.assertIn(b"error", r.stderr)

    def test_help(self):
        r = self.run_cli(["--help"])
        self.assertEqual(r.returncode, 0)
        self.assertIn(b"--indent", r.stdout)


if __name__ == "__main__":
    unittest.main()
