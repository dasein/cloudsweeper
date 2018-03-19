package notify

import (
	"brkt/cloudsweeper/cloud"
	"brkt/cloudsweeper/cloud/billing"
	"brkt/cloudsweeper/cloud/filter"
	"fmt"
	"log"
	"time"
)

const (
	smtpUserKey          = "SMTP_USER"
	smtpPassKey          = "SMTP_PASS"
	mailDisplayName      = "HouseKeeper"
	monthToDateAddressee = "eng@brkt.com"
)

type resourceMailData struct {
	Owner     string
	OwnerID   string
	Instances []cloud.Instance
	Images    []cloud.Image
	Snapshots []cloud.Snapshot
	Volumes   []cloud.Volume
	Buckets   []cloud.Bucket
}

type monthToDateData struct {
	CSP              cloud.CSP
	TotalCost        float64
	SortedUsers      billing.UserList
	MinimumTotalCost float64
	MinimumCost      float64
	AccountToUser    map[string]string
}

func (d *resourceMailData) ResourceCount() int {
	return len(d.Images) + len(d.Instances) + len(d.Snapshots) + len(d.Volumes) + len(d.Buckets)
}

// OldResourceReview will review (but not do any cleanup action) old resources
// that an owner might want to consider doing something about. The owner is then
// sent an email with a list of these resources. Resources are sent for review
// if they fulfil any of the following rules:
//		- Resource is older than 30 days
//		- A whitelisted resource is older than 6 months
//		- An instance marked with do-not-delete is older than a week
func OldResourceReview(mngr cloud.ResourceManager, accountUserMapping map[string]string) {
	allCompute := mngr.AllResourcesPerAccount()
	allBuckets := mngr.BucketsPerAccount()
	for account, resources := range allCompute {
		log.Println("Performing old resource review in", account)
		ownerName := convertEmailExceptions(accountUserMapping[account])

		// Create filters
		generalFilter := filter.New()
		generalFilter.AddGeneralRule(filter.OlderThanXDays(30))

		whitelistFilter := filter.New()
		whitelistFilter.OverrideWhitelist = true
		whitelistFilter.AddGeneralRule(filter.OlderThanXMonths(6))

		// These only apply to instances
		dndFilter := filter.New()
		dndFilter.AddGeneralRule(filter.HasTag("no-not-delete"))
		dndFilter.AddGeneralRule(filter.OlderThanXDays(7))

		dndFilter2 := filter.New()
		dndFilter2.AddGeneralRule(filter.NameContains("do-not-delete"))
		dndFilter2.AddGeneralRule(filter.OlderThanXDays(7))

		// Apply filters
		mailHolder := resourceMailData{
			Owner:     ownerName,
			Instances: filter.Instances(resources.Instances, generalFilter, whitelistFilter, dndFilter, dndFilter2),
			Images:    filter.Images(resources.Images, generalFilter, whitelistFilter),
			Volumes:   filter.Volumes(resources.Volumes, generalFilter, whitelistFilter),
			Snapshots: filter.Snapshots(resources.Snapshots, generalFilter, whitelistFilter),
			Buckets:   []cloud.Bucket{},
		}
		if bucks, ok := allBuckets[account]; ok {
			mailHolder.Buckets = filter.Buckets(bucks, generalFilter, whitelistFilter)
		}

		if mailHolder.ResourceCount() > 0 {
			// Now send email
			mailClient := getMailClient()
			mailContent, err := generateMail(mailHolder, reviewMailTemplate)
			if err != nil {
				log.Fatalln("Could not generate email:", err)
			}
			ownerMail := fmt.Sprintf("%s@brkt.com", mailHolder.Owner)
			log.Printf("Sending out old resource review to %s\n", ownerMail)
			title := fmt.Sprintf("You have %d old resources to review (%s)", mailHolder.ResourceCount(), time.Now().Format("2006-01-02"))
			err = mailClient.SendEmail(title, mailContent, ownerMail)
			if err != nil {
				log.Printf("Failed to email %s: %s\n", ownerMail, err)
			}
		}
	}
}

// UntaggedResourcesReview will look for resources without any tags, and
// send out a mail encouraging to tag tag them
func UntaggedResourcesReview(mngr cloud.ResourceManager, accountUserMapping map[string]string) {
	// We only care about untagged resources in EC2
	allCompute := mngr.AllResourcesPerAccount()
	for account, resources := range allCompute {
		log.Printf("Performing untagged resources review in %s", account)
		untaggedFilter := filter.New()
		untaggedFilter.AddGeneralRule(filter.Negate(filter.HasTag("Name")))

		// We care about un-tagged whitelisted resources too
		untaggedFilter.OverrideWhitelist = true

		ownerName := convertEmailExceptions(accountUserMapping[account])
		mailHolder := resourceMailData{
			Owner:     ownerName,
			OwnerID:   account,
			Instances: filter.Instances(resources.Instances, untaggedFilter),
			// Only report on instances for now
			//Images:    filter.Images(resources.Images, untaggedFilter),
			//Snapshots: filter.Snapshots(resources.Snapshots, untaggedFilter),
			//Volumes:   filter.Volumes(resources.Volumes, untaggedFilter),
			Buckets: []cloud.Bucket{},
		}

		if mailHolder.ResourceCount() > 0 {
			// Send mail
			mailClient := getMailClient()
			mailContent, err := generateMail(mailHolder, untaggedMailTemplate)
			if err != nil {
				log.Fatalf("Could not generate email: %s", err)
			}
			ownerMail := fmt.Sprintf("%s@brkt.com", mailHolder.Owner)
			log.Printf("Sending out untagged resource review to %s\n", ownerMail)
			title := fmt.Sprintf("You have %d un-tagged resources to review (%s)", mailHolder.ResourceCount(), time.Now().Format("2006-01-02"))
			err = mailClient.SendEmail(title, mailContent, ownerMail, "hsson@brkt.com", "ben@brkt.com") // # TODO: Remove temporary mails to hsson and ben
			if err != nil {
				log.Printf("Failed to email %s: %s\n", ownerMail, err)
			}
		}
	}
}

// DeletionWarning will find resources which are about to be deleted within
// `hoursInAdvance` hours, and send an email to the owner of those resources
// with a warning. Resources explicitly tagged to be deleted are not included
// in this warning.
func DeletionWarning(hoursInAdvance int, mngr cloud.ResourceManager, accountUserMapping map[string]string) {
	allCompute := mngr.AllResourcesPerAccount()
	allBuckets := mngr.BucketsPerAccount()
	for account, resources := range allCompute {
		ownerName := convertEmailExceptions(accountUserMapping[account])
		fil := filter.New()
		fil.AddGeneralRule(filter.DeleteWithinXHours(hoursInAdvance))
		mailHolder := struct {
			resourceMailData
			Hours int
		}{
			resourceMailData{
				ownerName,
				account,
				filter.Instances(resources.Instances, fil),
				filter.Images(resources.Images, fil),
				filter.Snapshots(resources.Snapshots, fil),
				filter.Volumes(resources.Volumes, fil),
				[]cloud.Bucket{},
			},
			hoursInAdvance,
		}
		if bucks, ok := allBuckets[account]; ok {
			mailHolder.Buckets = filter.Buckets(bucks, fil)
		}

		if mailHolder.ResourceCount() > 0 {
			// Now send email
			mailClient := getMailClient()
			mailContent, err := generateMail(mailHolder, deletionWarningTemplate)
			if err != nil {
				log.Fatalln("Could not generate email:", err)
			}
			ownerMail := fmt.Sprintf("%s@brkt.com", mailHolder.Owner)
			log.Printf("Warning %s about resource deletion\n", ownerMail)
			title := fmt.Sprintf("Deletion warning, %d resources are cleaned up within %d hours", mailHolder.ResourceCount(), hoursInAdvance)
			err = mailClient.SendEmail(title, mailContent, ownerMail, "hsson@brkt.com", "ben@brkt.com") // TODO: Remove tmp emails to hsson and ben
			if err != nil {
				log.Printf("Failed to email %s: %s\n", ownerMail, err)
			}
		}
	}
}

// MonthToDateReport sends an email to engineering with the
// Month-to-Date billing report
func MonthToDateReport(report billing.Report, accountUserMapping map[string]string) {
	mailClient := getMailClient()
	reportData := monthToDateData{report.CSP, report.TotalCost(), report.SortedUsersByTotalCost(), billing.MinimumTotalCost, billing.MinimumCost, accountUserMapping}
	mailContent, err := generateMail(reportData, monthToDateTemplate)
	if err != nil {
		log.Fatalln("Could not generate email:", err)
	}
	log.Printf("Sending the Month-to-date report to %s\n", monthToDateAddressee)
	title := fmt.Sprintf("Month-to-date %s billing report", report.CSP)
	err = mailClient.SendEmail(title, mailContent, monthToDateAddressee)
	if err != nil {
		log.Printf("Failed to email %s: %s\n", monthToDateAddressee, err)
	}
}
