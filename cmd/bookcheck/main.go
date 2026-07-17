package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	linkPattern    = regexp.MustCompile(`\[[^]]+\]\(([^)]+)\)`)
	chapterPattern = regexp.MustCompile(`^## 第([0-9]+)章`)
	filePattern    = regexp.MustCompile(`^([0-9]{2})-.*\.md$`)
)

func main() {
	root := "."
	if len(os.Args) == 2 {
		root = os.Args[1]
	}
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: bookcheck [book-root]")
		os.Exit(2)
	}

	if err := check(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("bookcheck: ok")
}

func check(root string) error {
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	summary, err := os.ReadFile(filepath.Join(root, "SUMMARY.md"))
	if err != nil {
		return err
	}

	var problems []string
	for _, path := range files {
		contents, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		problems = append(problems, checkLinks(path, contents)...)
		problems = append(problems, checkFences(path, contents)...)
		problems = append(problems, checkChapter(path, contents, summary)...)
	}

	if len(problems) != 0 {
		return errors.New("bookcheck failed:\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

func checkLinks(source string, contents []byte) []string {
	var problems []string
	dir := filepath.Dir(source)
	scanner := bufio.NewScanner(strings.NewReader(string(contents)))
	inFence := false
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}

		line = stripInlineCode(line)
		for _, match := range linkPattern.FindAllStringSubmatch(line, -1) {
			target := match[1]
			if hash := strings.IndexByte(target, '#'); hash >= 0 {
				target = target[:hash]
			}
			if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target = strings.TrimPrefix(target, "./")
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(target))); err != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: missing link target %q", source, lineNumber, target))
			}
		}
	}
	return problems
}

func stripInlineCode(line string) string {
	parts := strings.Split(line, "`")
	var builder strings.Builder
	for index := 0; index < len(parts); index += 2 {
		builder.WriteString(parts[index])
	}
	return builder.String()
}

func checkFences(path string, contents []byte) []string {
	scanner := bufio.NewScanner(strings.NewReader(string(contents)))
	open := false
	line := 0
	for scanner.Scan() {
		line++
		if strings.HasPrefix(strings.TrimSpace(scanner.Text()), "```") {
			open = !open
		}
	}
	if open {
		return []string{fmt.Sprintf("%s:%d: unclosed code fence", path, line)}
	}
	return nil
}

func checkChapter(path string, contents, summary []byte) []string {
	name := filepath.Base(path)
	match := filePattern.FindStringSubmatch(name)
	if match == nil {
		return nil
	}
	if !strings.Contains(string(summary), "("+"./"+name+")") {
		return []string{fmt.Sprintf("%s: not listed in SUMMARY.md", path)}
	}

	number, _ := strconv.Atoi(match[1])
	if number == 0 || strings.Contains(name, "附录") {
		return nil
	}
	heading := chapterPattern.FindSubmatch(contents)
	if heading == nil {
		return []string{fmt.Sprintf("%s: missing numbered chapter heading", path)}
	}
	headingNumber, _ := strconv.Atoi(string(heading[1]))
	if number != headingNumber {
		return []string{fmt.Sprintf("%s: filename chapter %d, heading chapter %d", path, number, headingNumber)}
	}
	return nil
}
