package main

import (
	"bufio"
	"bytes"
	"eximmon/exim"
	"eximmon/tools"
	"eximmon/whm"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var configPath = ".config"
var dataPath = "data/"

// date, id, <=, email, extras
var eximRegLine = regexp.MustCompile("(?i)([^ ]* [^ ]*) ([^ ]*) ([^ ]*) .* A=dovecot_plain:([^ ]*) (.*)$")
var notifyEmail = ""

func main() {
	logFile := "exim_mainlog"
	whm.ApiToken = os.Getenv("API_TOKEN")
	if whm.ApiToken == "" {
		log("Please declare -x API_TOKEN=...")
		log("Other environments variables: MAX_PER_MIN=4 , MAX_PER_HOUR=100")
		log("NOTIFY_EMAIL=email , EXIM_LOG=/var/log/exim_mainlog")
		log("WHM_API_HOST=127.0.0.1")
	}

	maxPerMin := int16(4)
	maxPerHour := int16(100)
	if os.Getenv("MAX_PER_MIN") != "" {
		if i, err := strconv.ParseInt(os.Getenv("MAX_PER_MIN"), 10, 16); err != nil {
			panic(fmt.Errorf("Failed parsing MAX_PER_MIN: %+v", err))
		} else {
			maxPerMin = int16(i)
		}
	}
	if os.Getenv("MAX_PER_HOUR") != "" {
		if i, err := strconv.ParseInt(os.Getenv("MAX_PER_HOUR"), 10, 16); err != nil {
			panic(fmt.Errorf("Failed parsing MAX_PER_HOUR: %+v", err))
		} else {
			maxPerHour = int16(i)
		}
	}

	if maxPerHour < maxPerMin {
		panic(fmt.Errorf("Max per hour must be above max per minutes"))
	}

	if os.Getenv("EXIM_LOG") != "" {
		logFile = os.Getenv("EXIM_LOG")
	}

	if os.Getenv("NOTIFY_EMAIL") != "" {
		notifyEmail = os.Getenv("NOTIFY_EMAIL")
	}

	if os.Getenv("WHM_API_HOST") != "" {
		whm.ApiHost = os.Getenv("WHM_API_HOST")
	}

	whm.Log = log

	if len(os.Args) < 2 {
		log("args: start|run|skip|reset|suspend|unsuspend|info|help|test-notify")
		return
	}

	maxRun := -1
	now := time.Now()
	//start from yesterday min
	startTime := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, now.Local().Location())
	switch os.Args[1] {
	case "reset":
		log("Removing %s*", dataPath)
		tools.RemoveSubFileFolder(dataPath)
		os.Remove(configPath)
		log("Removed %s", configPath)
		return
	case "start":
		//use yesterday
	case "run":
		maxRun = 1
	case "skip":
		startTime = time.Now() //skip to now, skip everything then...
	case "suspend":
		if len(os.Args) < 3 {
			log("suspend [email]")
			return
		}
		email := os.Args[2]
		if err := whm.SuspendEmail(email); err != nil {
			panic(fmt.Sprintf("error: %+v", err))
		}

		log("Suspended %s", email)
		return
	case "unsuspend":
		if len(os.Args) < 3 {
			log("unsuspend [email]")
			return
		}

		email := os.Args[2]
		if err := whm.UnSuspendEmail(email); err != nil {
			panic(fmt.Sprintf("error: %+v", err))
		}
		log("Unsuspended %s", email)

		return
	case "info":
		if len(os.Args) < 3 {
			log("info [domain]")
			return
		}
		info, err := whm.UserDataInfo(os.Args[2])
		if err != nil {
			panic(fmt.Sprintf("error: %+v", err))
		}
		log("%#v", info)
		return
	case "test-notify":
		if err := notifySuspend("test@example.com", "a test"); err != nil {
			log("notifySuspend error: %+v", err)
		}
		return

	case "help":
		log("start - continue from last position or start from yesterday, and repeats from last position")
		log("run - continue from last position or start from beginning for one time")
		log("skip - skip all existing data and repeats for new logs")
		log("reset - reset all data, huh, what?")
		log("suspend - suspend outgoing email")
		log("unsuspend - unsuspend outgoing email")
		log("info - get information of a domain")
		log("test-notify - test send notification mail")
		log("help - this!")
		return

	default:
		panic(fmt.Errorf("Unknown command: %s", os.Args[1]))
	}

	i := 1
	for {
		log("loop %d", i)
		if err := eximLogScanner(logFile, startTime, maxPerMin, maxPerHour); err != nil {
			log("log scanner error: %+v", err)
			// time.sleep(15 * time.Second)
		}

		if maxRun > -1 && i > maxRun {
			break
		}
		time.Sleep(15 * time.Second)
		i++
	} //loop

	log("Done.")
}

func eximLogScanner(logFile string, startTime time.Time, maxPerMin int16, maxPerHour int16) error {
	_, lastLine, lastPrefix, err := lastConfig(logFile)
	if err != nil {
		panic(err)
	}

	log("Scanning log from time: %v, last line %v", startTime.Format(time.RFC3339), lastLine)
	lastPrefix = strings.TrimRight(lastPrefix, "\n")
	newSize := MustSize(logFile)
	file, err := os.Open(logFile)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	//checks if line still valid
	scanner := bufio.NewScanner(file)
	lineNo := int64(1)
	for scanner.Scan() {
		if lineNo >= lastLine {
			break
		}
		// log("Skipping: %d: %d", lineNo, scanner.Text())
		lineNo++
	}

	text := scanner.Text()
	if lastLine > 0 {
		if !strings.HasPrefix(text, lastPrefix) {
			log("Line mismatch line:\n%s\nExpecting:\n%s\n", text, lastPrefix)
			time.Sleep(10 * time.Second)
			_, lastLine, lastPrefix = 0, 0, ""

			scanner = bufio.NewScanner(file) //reset scanner
			scanner.Scan()
			lineNo = 1
		} else {
			//skip to next line
			if !scanner.Scan() {
				log("No more line since last scan")
				return nil
			}
			lineNo++
		}
	}

	if err := scanner.Err(); err != nil {
		panic(err)
	}

	for {
		text = scanner.Text()
		res := eximRegLine.FindStringSubmatch(text)
		if len(res) < 5 {
			// log("Not: %v", text)

		} else {
			if res[3] == "<=" {
				email := res[4] //login owner
				process := false
				skipTime := false
				var thetime time.Time
				if strings.Index(email, "@") > 0 {
					process = true
					var err error
					//is email
					//received
					thetime, err = exim.ParseDate(res[1])
					if err != nil {
						panic(fmt.Errorf("Unable to read date: %#v on line %d", thetime, lineNo))
					}
					if !startTime.IsZero() {
						if thetime.Before(startTime) {
							log("Skipping by time %s expected %s", thetime.Format(time.RFC3339), startTime.Format(time.RFC3339))
							process = false
							skipTime = true
						}
					}
				} //is email

				if process {
					minCount, hourCount, err := mailCount(thetime, email)
					if err != nil {
						return err
					}
					minCount++
					hourCount++
					//TODO save based on X recipients per email?

					if err := mailCountStore(thetime, email, hourCount, minCount); err != nil {
						panic(fmt.Errorf("Unable to save count %s, time: %#v, error: %#v", email, thetime, err))
					}

					if minCount > int64(maxPerMin) || hourCount > int64(maxPerHour) {
						if err := whm.SuspendEmail(email); err != nil {
							log("Unable to suspendEmail %s, error: %+v", email, err)
							time.Sleep(5 * time.Second)
						}

						if notifyEmail != "" {
							if err = notifySuspend(email, fmt.Sprintf("Count: minute: %d, hour: %d", minCount, hourCount)); err != nil {
								log("notifySuspend error: %+v", err)
								time.Sleep(10 * time.Second)
							}
						}
					}

					log("Written %s time: %v, min: %v, hour: %v", email, thetime, minCount, hourCount)
				} else if !skipTime {
					log("Ignoring %s", text)
					time.Sleep(2 * time.Second)
				}
			} //is <=
		}
		if !scanner.Scan() {
			break
		}
		lineNo++
	}

	log("ended: line %d", lineNo)

	lastPrefix = text[0:25]

	storeConfig(logFile, newSize, lineNo, lastPrefix)
	return nil
}

func notifySuspend(email string, message string) error {
	if notifyEmail == "" {
		return fmt.Errorf("NOTIFY_EMAIL not set")
	}

	c1 := exec.Command("echo", "-e", fmt.Sprintf("\"%s\"", message))
	c2 := exec.Command("mail", "-s", fmt.Sprintf("\"suspended email %s\"", email), notifyEmail)
	r, w := io.Pipe()
	c1.Stdout = w
	c2.Stdin = r
	var b2 bytes.Buffer
	c2.Stdout = &b2

	if err := c1.Start(); err != nil {
		return err
	}

	if err := c2.Start(); err != nil {
		return err
	}

	if err := c1.Wait(); err != nil {
		return err
	}

	w.Close()

	if err := c2.Wait(); err != nil {
		return err
	}

	io.Copy(os.Stdout, &b2)

	log("mail-result: %s", b2.Bytes())
	return nil
}

func mailCountStore(thetime time.Time, email string, hourCount int64, minCount int64) error {
	path := dataPath + cleanPath(email)

	dirPath := cleanPath(thetime.Format("2006-01-02"))
	hourPath := thetime.Format("15")
	minPath := thetime.Format("1504")

	datePath := path + "/" + dirPath
	hourFile := datePath + "/" + hourPath
	minFile := datePath + "/" + minPath

	MustDir(datePath)

	log("Writing %s", hourFile)
	if err := ioutil.WriteFile(hourFile, []byte(fmt.Sprintf("%d", hourCount)), 0644); err != nil {
		return err
	}
	log("Writing %s", minFile)
	if err := ioutil.WriteFile(minFile, []byte(fmt.Sprintf("%d", minCount)), 0644); err != nil {
		return err
	}
	return nil
}

// this minute, this hour count
func mailCount(thetime time.Time, email string) (int64, int64, error) {
	path := dataPath + cleanPath(email)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// path/to/whatever does not exist
		return 0, 0, nil
	}

	dirPath := cleanPath(thetime.Format("2006-01-02"))
	// hourPath := now.Format("150405")
	hourPath := thetime.Format("15")
	minPath := thetime.Format("1504")

	datePath := path + "/" + dirPath
	if _, err := os.Stat(datePath); os.IsNotExist(err) {
		// path/to/whatever does not exist
		return 0, 0, nil
	}

	if _, err := os.Stat(datePath); os.IsNotExist(err) {
		// path/to/whatever does not exist
		return 0, 0, nil
	}

	hourCount := int64(0)
	minCount := int64(0)
	hourFile := datePath + "/" + hourPath
	minFile := datePath + "/" + minPath

	if _, err := os.Stat(hourFile); !os.IsNotExist(err) {
		content, err := ioutil.ReadFile(hourFile)
		if err != nil {
			return 0, 0, err
		}
		hourCount, err = strconv.ParseInt(string(content), 0, 64)
		if err != nil {
			return 0, 0, err
		}
	}
	if _, err := os.Stat(minFile); !os.IsNotExist(err) {
		content, err := ioutil.ReadFile(minFile)
		if err != nil {
			return 0, 0, err
		}
		minCount, err = strconv.ParseInt(string(content), 0, 64)
		if err != nil {
			return 0, 0, err
		}
	}

	return minCount, hourCount, nil
}

func storeConfig(logFile string, size int64, line int64, prefix string) error {
	return ioutil.WriteFile(configPath, []byte(fmt.Sprintf("%d||%d||%s", size, line, prefix)), 0644)
}

//last size, last line#, prefix
func lastConfig(logFile string) (int64, int64, string, error) {
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// path/to/whatever does not exist
		return 0, 0, "", nil
	}

	content, err := ioutil.ReadFile(configPath)
	if err != nil {
		return 0, 0, "", err
	}

	args := strings.Split(string(content), "||")
	size, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, 0, "", err
	}
	line, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return 0, 0, "", err
	}
	prefix := args[2]

	newSize := MustSize(logFile)
	if newSize < size {
		log("Size shrinked: %f, expected: %f", newSize, size)
		time.Sleep(10 * time.Second)
		return 0, 0, "", nil //reset
	}

	return size, line, prefix, nil
}

func cleanPath(name string) string {
	res := strings.Replace(name, "@", "_", -1)
	res = strings.Replace(res, "-", "_", -1)
	res = filepath.Clean(res)
	return res
}

func MustDir(path string) {
	log("MustDir: %s", path)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		log("Creating dir: %s", path)
		if err := os.MkdirAll(path, 0744); err != nil {
			panic(fmt.Errorf("Unable to create %s, error: %+v", path, err))
		}
	} else if err != nil {
		panic(fmt.Errorf("Unknown mustdir %s, error: %+v", path, err))
	}
}

func MustSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		panic(fmt.Errorf("Unable to state %s, error: %+v", path, err))
	}

	return fi.Size()
}

func log(msg string, args ...interface{}) {
	fmt.Printf(msg+"\n", args...)
}