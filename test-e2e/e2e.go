package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"strings"

	yaml "gopkg.in/yaml.v2"
	"k8s.io/klog"

	reg "sigs.k8s.io/k8s-container-image-promoter/lib/dockerregistry"
	"sigs.k8s.io/k8s-container-image-promoter/lib/stream"
)

// GitDescribe is stamped by bazel.
var GitDescribe string

// GitCommit is stamped by bazel.
var GitCommit string

// TimestampUtcRfc3339 is stamped by bazel.
var TimestampUtcRfc3339 string

// nolint[lll]
func main() {
	// NOTE: We can't run the tests in parallel because we only have 1 pair of
	// staging/prod GCRs.

	testsPtr := flag.String(
		"tests", "", "the e2e tests file (YAML) to load (REQUIRED)")
	repoRootPtr := flag.String(
		"repo-root", "", "the absolute path of the CIP git repository on disk")
	keyFilePtr := flag.String(
		"key-file", "", "the .json key file to use to activate the service account in the tests (tests only support using 1 service account)")
	helpPtr := flag.Bool(
		"help",
		false,
		"print help")

	flag.Parse()

	if len(os.Args) == 1 {
		printVersion()
		printUsage()
		os.Exit(0)
	}

	if *helpPtr {
		printUsage()
		os.Exit(0)
	}

	if *testsPtr == "" {
		klog.Fatal(fmt.Errorf("-tests=... flag is required"))
	}

	if *repoRootPtr == "" {
		klog.Fatal(fmt.Errorf("-repo-root=... flag is required"))
	}

	ts, err := readE2ETests(*testsPtr)
	if err != nil {
		klog.Fatal(err)
	}

	if len(*keyFilePtr) > 0 {
		err = activateServiceAccount(*keyFilePtr)
		if err != nil {
			klog.Fatal("could not activate service account from .json", err)
		}
	}

	// Loop through each e2e test case.
	for _, t := range ts {
		fmt.Printf("Running e2e test '%s'...\n", t.Name)
		err := testSetup(*repoRootPtr, t.Manifest)
		if err != nil {
			klog.Fatal("error with test setup:", err)
		}

		for _, snapshot := range t.Snapshots {
			images, err := getSnapshot(*repoRootPtr, snapshot.Name, t.Manifest)
			if err != nil {
				klog.Fatal("could not get pre-promotion snapshot of repo", snapshot.Name)
			}
			if err := checkEqual(snapshot.Before, images); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		}

		manifestPath, err := writeTempManifest(t.Manifest)
		if err != nil {
			klog.Fatal("could not write Manifest file:", err)
		}
		err = runPromotion(*repoRootPtr, manifestPath)
		if err != nil {
			klog.Fatal("error with promotion:", err)
		}
		for _, snapshot := range t.Snapshots {
			images, err := getSnapshot(*repoRootPtr, snapshot.Name, t.Manifest)
			if err != nil {
				klog.Fatal("could not get post-promotion snapshot of repo", snapshot.Name)
			}
			if err := checkEqual(snapshot.After, images); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		}
		fmt.Printf("e2e test '%s': OK\n", t.Name)
	}
}

func activateServiceAccount(keyFilePath string) error {
	cmd := exec.Command("gcloud",
		"auth",
		"activate-service-account",
		"--key-file="+keyFilePath)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		fmt.Println("stdout", stdout.String())
		fmt.Println("stderr", stderr.String())
		return err
	}

	fmt.Println(stdout.String())
	return nil
}

func testSetup(cwd string, mfest reg.Manifest) error {

	sc, err := reg.MakeSyncContext(
		[]reg.Manifest{mfest},
		2,
		10,
		false,
		true)
	if err != nil {
		klog.Fatal(err)
	}

	err = sc.PopulateTokens()
	if err != nil {
		klog.Fatal(err)
	}

	sc.ReadRegistries(
		sc.RegistryContexts,
		// Read all registries recursively, because we want to delete every
		// image found in it (clearRepository works by deleting each image found
		// in sc.Inv).
		true,
		reg.MkReadRepositoryCmdReal)

	// Clear ALL registries in the test manifest. Blank slate!
	for _, rc := range mfest.Registries {
		fmt.Println("CLEARING REPO", rc.Name)
		clearRepository(rc.Name, mfest, &sc)
	}

	pwd := os.Getenv("PWD")
	pushRepo := getBazelOption("STABLE_TEST_STAGING_IMG_REPOSITORY")

	if pushRepo == "" {
		return fmt.Errorf(
			"could not dereference STABLE_TEST_STAGING_IMG_REPOSITORY")
	}

	cmds := [][]string{
		{
			"bazel",
			"build",
			"--host_force_python=PY2",
			fmt.Sprintf(
				"--workspace_status_command=%s/workspace_status.sh",
				pwd),
			"//test-e2e:golden-images-loadable.tar",
		},
		// In order to create a manifest list, images must be pushed to a
		// repository first.
		{
			"bazel",
			"run",
			"--host_force_python=PY2",
			fmt.Sprintf(
				"--workspace_status_command=%s/workspace_status.sh",
				pwd),
			"//test-e2e:push-golden",
		},
		{
			"docker",
			"manifest",
			"create",
			fmt.Sprintf("%s/golden:1.0", pushRepo),
			fmt.Sprintf("%s/golden:1.0-linux_amd64", pushRepo),
			fmt.Sprintf("%s/golden:1.0-linux_s390x", pushRepo),
		},
		// Fixup the s390x image because it's set to amd64 by default (there is
		// no way to specify architecture from within bazel yet when creating
		// images).
		{
			"docker",
			"manifest",
			"annotate",
			"--arch=s390x",
			fmt.Sprintf("%s/golden:1.0", pushRepo),
			fmt.Sprintf("%s/golden:1.0-linux_s390x", pushRepo),
		},
		{
			"docker",
			"manifest",
			"inspect",
			fmt.Sprintf("%s/golden:1.0", pushRepo),
		},
		// Finally, push the manifest list. It is just metadata around existing
		// images in a repository.
		{
			"docker",
			"manifest",
			"push",
			"--purge",
			fmt.Sprintf("%s/golden:1.0", pushRepo),
		},
	}

	for _, cmd := range cmds {
		fmt.Println("execing cmd", cmd)
		stdout, stderr, err := execCommand(cwd, cmd[0], cmd[1:]...)
		if err != nil {
			return err
		}
		fmt.Println(stdout)
		fmt.Println(stderr)
	}

	return nil
}

func execCommand(
	cwd, cmdString string,
	args ...string) (string, string, error) {

	cmd := exec.Command(cmdString, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		klog.Errorf("for command %s:\nstdout:\n%sstderr:\n%s\n",
			cmdString,
			stdout.String(),
			stderr.String())
		return "", "", err
	}
	return stdout.String(), stderr.String(), nil
}

func getBazelOption(o string) string {
	stdout, _, err := execCommand(
		"",
		fmt.Sprintf("%s/workspace_status.sh", os.Getenv("PWD")))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if strings.Contains(line, o) {
			words := strings.Split(line, " ")
			if len(words) == 2 {
				return words[1]
			}
		}
	}
	return ""
}

func writeTempManifest(mfest reg.Manifest) (string, error) {
	bs, err := yaml.Marshal(mfest)
	if err != nil {
		return "", fmt.Errorf("cannot serialize manifest to yaml: %s", err)
	}

	tmpfile, err := ioutil.TempFile("", "tmp-promoter-manifest")
	if err != nil {
		klog.Fatal(err)
	}

	if _, err := tmpfile.Write(bs); err != nil {
		klog.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		klog.Fatal(err)
	}

	return tmpfile.Name(), nil
}

func runPromotion(cwd string, manifestPath string) error {
	// nolint[errcheck]
	defer os.Remove(manifestPath)

	// NOTE: we should probably compile the cip binary and then put it in the
	// PATH and then use multirun.sh as-is.

	cmd := exec.Command(
		"bazel",
		"run",
		"--workspace_status_command="+cwd+"/workspace_status.sh",
		":cip",
		"--",
		"-dry-run=false",
		"-verbosity=3",
		"-manifest="+manifestPath)

	cmd.Dir = cwd

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		fmt.Println("stdout", stdout.String())
		fmt.Println("stderr", stderr.String())
		return err
	}

	fmt.Println(stdout.String())
	return nil
}

func extractSvcAcc(registry reg.RegistryName, mfest reg.Manifest) string {
	for _, r := range mfest.Registries {
		if r.Name == registry {
			return r.ServiceAccount
		}
	}
	return ""
}

func getSnapshot(cwd string,
	registry reg.RegistryName,
	mfest reg.Manifest) ([]reg.Image, error) {

	invocation := []string{
		"run",
		"--workspace_status_command=" + cwd + "/workspace_status.sh",
		":cip",
		"--",
		"-snapshot=" + string(registry)}

	svcAcc := extractSvcAcc(registry, mfest)
	if len(svcAcc) > 0 {
		invocation = append(invocation, "-snapshot-service-account="+svcAcc)
	}

	cmd := exec.Command("bazel", invocation...)

	cmd.Dir = cwd

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		fmt.Println("stdout", stdout.String())
		fmt.Println("stderr", stderr.String())
		return nil, err
	}

	images := make([]reg.Image, 0)
	if err = yaml.UnmarshalStrict(stdout.Bytes(), &images); err != nil {
		return nil, err
	}
	return images, err
}

func clearRepository(regName reg.RegistryName,
	mfest reg.Manifest,
	sc *reg.SyncContext) {

	mkDeletionCmd := func(
		dest reg.RegistryContext,
		imageName reg.ImageName,
		digest reg.Digest) stream.Producer {
		var sp stream.Subprocess
		sp.CmdInvocation = reg.GetDeleteCmd(
			dest,
			sc.UseServiceAccount,
			imageName,
			digest,
			true)
		return &sp
	}

	sc.ClearRepository(regName, mfest, mkDeletionCmd, nil)
}

// E2ETest holds all the information about a single e2e test. It has the
// promoter manifest, and the before/after snapshots of all repositories that it
// cares about.
type E2ETest struct {
	Name      string             `name:"tests,omitempty"`
	Manifest  reg.Manifest       `manifest:"tests,omitempty"`
	Snapshots []RegistrySnapshot `snapshots:"tests,omitempty"`
}

// E2ETests is an array of E2ETest.
type E2ETests []E2ETest

// RegistrySnapshot is the snapshot of a registry. It is basically the key/value
// pair out of the reg.MasterInventory type (RegistryName + []Image).
type RegistrySnapshot struct {
	Name   reg.RegistryName `yaml:"name,omitempty"`
	Before []reg.Image      `yaml:"before,omitempty"`
	After  []reg.Image      `yaml:"after,omitempty"`
}

func readE2ETests(filePath string) (E2ETests, error) {
	var ts E2ETests
	b, err := ioutil.ReadFile(filePath)
	if err != nil {
		return ts, err
	}
	if err := yaml.UnmarshalStrict(b, &ts); err != nil {
		return ts, err
	}

	return ts, nil
}

func printVersion() {
	fmt.Printf("Built:   %s\n", TimestampUtcRfc3339)
	fmt.Printf("Version: %s\n", GitDescribe)
	fmt.Printf("Commit:  %s\n", GitCommit)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

// TODO: Use the version of checkEqual found in
// lib/dockerregistry/inventory_test.go.
func checkEqual(got, expected interface{}) error {
	if !reflect.DeepEqual(got, expected) {
		return fmt.Errorf(
			`<<<<<<< got (type %T)
%v
=======
%v
>>>>>>> expected (type %T)`,
			got,
			got,
			expected,
			expected)
	}
	return nil
}