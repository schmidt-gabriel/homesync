import Testing

@testable import HomeSyncKit

@Suite("Ignore rules")
struct IgnoreRulesTests {
    @Test("macOS noise is excluded even with no server rules")
    func platformNoiseAlwaysApplies() {
        let rules = IgnoreRules(rules: "")

        // The server's list can be edited from any machine. If someone empties
        // it, this one must still not start uploading .DS_Store everywhere.
        #expect(rules.excludes(".DS_Store"))
        #expect(rules.excludes("projects/alpha/.DS_Store"))
        #expect(rules.excludes("._resourcefork"))
        #expect(rules.excludes("notes.swp"))
        #expect(rules.excludes(".Spotlight-V100"))
        #expect(rules.excludes("upload-1234.homesync-tmp"))

        #expect(!rules.excludes("notes.md"))
        #expect(!rules.excludes("projects/alpha/notes.md"))
    }

    @Test("a bare name matches at any depth")
    func bareNameMatchesAnywhere() {
        let rules = IgnoreRules(rules: "secret.txt")

        #expect(rules.excludes("secret.txt"))
        #expect(rules.excludes("a/secret.txt"))
        #expect(rules.excludes("a/b/c/secret.txt"))
        #expect(!rules.excludes("not-secret.txt"))
    }

    @Test("a pattern with a slash is anchored to the whole path")
    func slashAnchorsToFullPath() {
        let rules = IgnoreRules(rules: "build/output.bin")

        #expect(rules.excludes("build/output.bin"))
        // Anchored: the same name deeper in the tree is a different path.
        #expect(!rules.excludes("a/build/output.bin"))
    }

    @Test("a leading slash anchors to the root")
    func leadingSlashAnchors() {
        let rules = IgnoreRules(rules: "/notes.md")

        #expect(rules.excludes("notes.md"))
        #expect(!rules.excludes("projects/notes.md"))
    }

    @Test("globs match within a segment")
    func globsMatchWithinSegment() {
        let rules = IgnoreRules(rules: "*.log")

        #expect(rules.excludes("server.log"))
        #expect(rules.excludes("logs/server.log"))
        #expect(!rules.excludes("server.log.keep"))
    }

    @Test("single-star does not cross directories, double-star does")
    func starDepth() {
        let shallow = IgnoreRules(rules: "build/*.o")
        #expect(shallow.excludes("build/main.o"))
        #expect(!shallow.excludes("build/deep/main.o"))

        let deep = IgnoreRules(rules: "build/**/*.o")
        #expect(deep.excludes("build/deep/main.o"))
        #expect(deep.excludes("build/very/deep/main.o"))
    }

    @Test("a trailing slash restricts a rule to directories")
    func directoryOnlyRules() {
        let rules = IgnoreRules(rules: "cache/")

        #expect(rules.excludes("cache", isDirectory: true))
        // A *file* named cache is not what the rule meant.
        #expect(!rules.excludes("cache", isDirectory: false))
    }

    @Test("everything inside an ignored directory is ignored")
    func ignoredDirectoriesCoverTheirContents() {
        let rules = IgnoreRules(rules: "build/")

        // Without this, `build/` would keep the directory out of the index
        // while its contents streamed up one by one.
        #expect(rules.excludes("build/main.o"))
        #expect(rules.excludes("build/deep/nested/main.o"))
        #expect(!rules.excludes("src/main.swift"))
    }

    @Test("a later negation re-includes")
    func negationReincludes() {
        let rules = IgnoreRules(rules: """
            *.log
            !important.log
            """)

        #expect(rules.excludes("server.log"))
        #expect(!rules.excludes("important.log"))
    }

    @Test("order decides: a negation before its rule does not win")
    func negationOrderMatters() {
        let rules = IgnoreRules(rules: """
            !important.log
            *.log
            """)

        #expect(rules.excludes("important.log"))
    }

    @Test("blank lines and comments are skipped")
    func commentsAreIgnored() {
        let rules = IgnoreRules(rules: """
            # a comment
              # an indented comment

            *.tmp
            """)

        #expect(rules.excludes("scratch.tmp"))
        #expect(!rules.excludes("a comment"))
        #expect(!rules.excludes("#"))
    }

    @Test("an empty rule set still parses")
    func emptyRules() {
        let rules = IgnoreRules(rules: "\n\n   \n")
        #expect(!rules.excludes("notes.md"))
    }
}
