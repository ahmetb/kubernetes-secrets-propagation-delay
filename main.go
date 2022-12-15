package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	log.SetOutput(os.Stderr)
	secretName := "my-secret"
	secretKey := "time"
	podName := "my-pod"
	if err := deleteSecret(secretName); err != nil {
		panic(err)
	}
	if err := applySecret(secretName, secretKey, "initial-value"); err != nil {
		panic(err)
	}
	if err := makePod(podName, secretName); err != nil {
		panic(err)
	}
	podVals, err := startPodSecretWatch(podName, secretKey)
	if err != nil {
		panic(err)
	}

	updateSecret := make(chan struct{})
	appliedSecrets := startSecretUpdates(podName, secretName, secretKey, updateSecret)
	updateSecret <- struct{}{}

	var lastSecretUpdate, lastPodVal time.Time
	fmt.Println("now,last_secret_update,last_on_pod")
	for {
		now := time.Now()
		var ok bool
		select {
		case lastSecretUpdate = <-appliedSecrets:
		case v := <-podVals:
			intVal, err := strconv.Atoi(v)
			if err != nil {
				log.Printf("warn: failed to parse pod value as time %q: %v", v, err)
				continue
			}
			lastPodVal = time.Unix(int64(intVal), 0)

			if ok {
				fmt.Printf("%d,%d,%d\n", now.Unix(), lastSecretUpdate.Unix(), lastPodVal.Unix())
			}
			if lastPodVal == lastSecretUpdate {
				log.Printf("pod caught with last secret update (%v), took: %v", lastSecretUpdate.Unix(), now.Sub(lastSecretUpdate))
				updateSecret <- struct{}{}
			}
		}
	}
}

func startSecretUpdates(podName, secretName, key string, update <-chan struct{}) <-chan time.Time {
	out := make(chan time.Time)
	go func() {
		for range update {
			now := time.Now().Truncate(time.Second)
			v := fmt.Sprintf("%d", now.Unix())
			if err := applySecret(secretName, key, v); err != nil {
				log.Printf("warn: secret update fail: %v", err)
			}
			log.Println("secret updated with ", v)

			// If we annotate the pod after updating the Secret, the pod is re-synced
			// on kubelet, which seems to be prompting a remount of the Secret/ConfigMap
			// volume, which causes the volume to be rebuilt (and therefore
			// be up to date.
			//
			// if err := annotatePod(podName, "example.com/time-annotation", v); err != nil {
			// 	log.Printf("failed to annotate pod: %v", err)
			// }
			out <- now
		}
	}()
	return out
}

func annotatePod(podName, k, v string) error {
	_, err := kubectl("annotate", "pods", podName, k+"="+v, "--overwrite")
	return err
}

func deleteSecret(name string) error { return deleteResource("secret", name) }
func deletePod(name string) error    { return deleteResource("pod", name) }

func deleteResource(kind, name string) error {
	_, err := kubectl("delete", kind, name, "--ignore-not-found=true")
	return err
}

func applySecret(name, k, v string) error {
	yaml, err := kubectl("create", "secret", "generic", name, "--from-literal="+k+"="+v, "--dry-run=client", "-o=yaml")
	if err != nil {
		return err
	}
	_, err = kubectlWithStdin(strings.NewReader(yaml), "apply", "-f", "-")
	return err
}

func makePod(name, secretName string) error {
	if err := deletePod(name); err != nil {
		return err
	}
	podSpec := strings.NewReader(`
apiVersion: v1
kind: Pod
metadata:
  name: ` + name + `
  labels:
    app: my-app
spec:
  terminationGracePeriodSeconds: 0
  containers:
    - name: main
      image: busybox
      command: ["sleep", "9999999"]
      volumeMounts:
        - name: secret-volume
          mountPath: /secrets
          readOnly: true
  volumes:
    - name: secret-volume
      secret:
        secretName: ` + secretName)

	podJSON, err := kubectlWithStdin(podSpec, "apply", "-f-", "-o=yaml")
	if err != nil {
		return err
	}
	fmt.Println(podJSON)

	return waitPod(name)
}

func waitPod(name string) error {
	_, err := kubectl("wait", "--for=condition=Ready", "pod/"+name)
	return err
}

func kubectl(args ...string) (string, error) {
	return kubectlWithStdin(nil, args...)
}

func kubectlWithStdin(stdin io.Reader, args ...string) (string, error) {
	var o, b bytes.Buffer
	cmd := exec.Command("kubectl", args...)
	cmd.Stderr = &b
	cmd.Stdout = &o
	cmd.Stdin = stdin
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("kubectl %s failed:%w %s", args, err, b.String())
	}
	return o.String(), nil
}

func startPodSecretWatch(podName, secretKey string) (<-chan string, error) {
	cmd := exec.Command("kubectl", "exec", podName, "--",
		"sh", "-c", "while :; do echo $(cat /secrets/"+secretKey+"); sleep 1; done")
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = os.Stderr
	s := bufio.NewReader(pr)

	out := make(chan string)
	go func() {
		for {
			l, _, err := s.ReadLine()
			if err != nil {
				if err == io.EOF {
					break
				}
				out <- err.Error()
			} else {
				out <- string(l)
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return out, nil
}
