package cmd

import (
	"fmt"
	"time"

	"github.com/rebuy-de/aws-nuke/pkg/awsutil"
	"github.com/rebuy-de/aws-nuke/pkg/config"
	"github.com/rebuy-de/aws-nuke/pkg/types"
	"github.com/rebuy-de/aws-nuke/resources"
	"github.com/sirupsen/logrus"
)

type Nuke struct {
	Parameters NukeParameters
	Account    awsutil.Account
	Config     *config.Nuke

	ResourceTypes types.Collection

	items Queue
}

func NewNuke(params NukeParameters, account awsutil.Account) *Nuke {
	n := Nuke{
		Parameters: params,
		Account:    account,
	}

	return &n
}

func (n *Nuke) Run() error {
	var err error

	fmt.Printf("aws-nuke version %s - %s - %s\n\n", BuildVersion, BuildDate, BuildHash)

	err = n.Config.ValidateAccount(n.Account.ID(), n.Account.Aliases())
	if err != nil {
		return err
	}

	fmt.Printf("Nuking the account with the ID %s and the alias '%s'.\n", n.Account.ID(), n.Account.Alias())

	err = n.Scan()
	if err != nil {
		return err
	}

	if n.items.Count(ItemStateNew) == 0 {
		fmt.Println("No resource to delete.")
		return nil
	}

	if !n.Parameters.NoDryRun {
		fmt.Println("The above resources would be deleted with the supplied configuration. Provide --no-dry-run to actually destroy resources.")
		return nil
	}

	fmt.Printf("Nuking the resources on the account with the ID %s and the alias '%s'?\n", n.Account.ID(), n.Account.Alias())

	failCount := 0
	waitingCount := 0

	for {
		n.HandleQueue()

		if n.items.Count(ItemStatePending, ItemStateWaiting, ItemStateNew) == 0 && n.items.Count(ItemStateFailed) > 0 {
			if failCount >= 2 {
				logrus.Errorf("There are resources in failed state, but none are ready for deletion, anymore.")
				fmt.Println()

				for _, item := range n.items {
					if item.State != ItemStateFailed {
						continue
					}

					item.Print()
					logrus.Error(item.Reason)
				}

				return fmt.Errorf("failed")
			}

			failCount = failCount + 1
		} else {
			failCount = 0
		}
		if n.Parameters.MaxWaitRetries != 0 && n.items.Count(ItemStateWaiting, ItemStatePending) > 0 && n.items.Count(ItemStateNew) == 0 {
			if waitingCount >= n.Parameters.MaxWaitRetries {
				return fmt.Errorf("Max wait retries of %d exceeded.\n\n", n.Parameters.MaxWaitRetries)
			}
			waitingCount = waitingCount + 1
		} else {
			waitingCount = 0
		}
		if n.items.Count(ItemStateNew, ItemStatePending, ItemStateFailed, ItemStateWaiting) == 0 {
			break
		}

		time.Sleep(5 * time.Second)
	}

	fmt.Printf("Nuke complete: %d failed, %d skipped, %d finished.\n\n",
		n.items.Count(ItemStateFailed), n.items.Count(ItemStateFiltered), n.items.Count(ItemStateFinished))

	return nil
}

func (n *Nuke) Scan() error {
	targets := types.Collection{}
	excludes := types.Collection{}

	if acctCfg, ok := n.Config.Accounts[n.Account.ID()]; ok {
		targets = acctCfg.ResourceTypes.Targets
		excludes = acctCfg.ResourceTypes.Excludes
	} else if defaultCfg, ok := n.Config.Accounts["__default__"]; ok {
		targets = defaultCfg.ResourceTypes.Targets
		excludes = defaultCfg.ResourceTypes.Excludes
	}

	resourceTypes := ResolveResourceTypes(
		resources.GetListerNames(),
		[]types.Collection{
			n.Parameters.Targets,
			n.Config.ResourceTypes.Targets,
			targets,
		},
		[]types.Collection{
			n.Parameters.Excludes,
			n.Config.ResourceTypes.Excludes,
			excludes,
		},
	)

	queue := make(Queue, 0)

	for _, regionName := range n.Config.Regions {
		region := NewRegion(regionName, n.Account.ResourceTypeToServiceType, n.Account.NewSession)

		items := Scan(region, resourceTypes)
		for item := range items {
			ffGetter, ok := item.Resource.(resources.FeatureFlagGetter)
			if ok {
				ffGetter.FeatureFlags(n.Config.FeatureFlags)
			}

			queue = append(queue, item)
			err := n.Filter(item)
			if err != nil {
				return err
			}

			if item.State != ItemStateFiltered || !n.Parameters.Quiet {
				item.Print()
			}
		}
	}

	fmt.Printf("Scan complete: %d total, %d nukeable, %d filtered.\n\n",
		queue.CountTotal(), queue.Count(ItemStateNew), queue.Count(ItemStateFiltered))

	n.items = queue

	return nil
}

func (n *Nuke) Filter(item *Item) error {

	checker, ok := item.Resource.(resources.Filter)
	if ok {
		err := checker.Filter()
		if err != nil {
			item.State = ItemStateFiltered
			item.Reason = err.Error()

			// Not returning the error, since it could be because of a failed
			// request to the API. We do not want to block the whole nuking,
			// because of an issue on AWS side.
			return nil
		}
	}

	accountFilters, err := n.Config.Filters(n.Account.ID())
	if err != nil {
		return err
	}

	itemFilters, ok := accountFilters[item.Type]
	if !ok {
		return nil
	}

	for _, filter := range itemFilters {
		prop, err := item.GetProperty(filter.Property)

		match, err := filter.Match(prop)
		if err != nil {
			return err
		}

		if IsTrue(filter.Invert) {
			match = !match
		}

		if match {
			item.State = ItemStateFiltered
			item.Reason = "filtered by config"
			return nil
		}
	}

	return nil
}

func (n *Nuke) HandleQueue() {
	listCache := make(map[string]map[string][]resources.Resource)

	for _, item := range n.items {
		switch item.State {
		case ItemStateNew:
			n.HandleRemove(item)
			item.Print()
		case ItemStateFailed:
			n.HandleRemove(item)
			n.HandleWait(item, listCache)
			item.Print()
		case ItemStatePending:
			n.HandleWait(item, listCache)
			item.State = ItemStateWaiting
			item.Print()
		case ItemStateWaiting:
			n.HandleWait(item, listCache)
			item.Print()
		}

	}

	fmt.Println()
	fmt.Printf("Removal requested: %d waiting, %d failed, %d skipped, %d finished\n\n",
		n.items.Count(ItemStateWaiting, ItemStatePending), n.items.Count(ItemStateFailed),
		n.items.Count(ItemStateFiltered), n.items.Count(ItemStateFinished))
}

func (n *Nuke) HandleRemove(item *Item) {
	err := item.Resource.Remove()
	if err != nil {
		item.State = ItemStateFailed
		item.Reason = err.Error()
		return
	}

	item.State = ItemStatePending
	item.Reason = ""
}

func (n *Nuke) HandleWait(item *Item, cache map[string]map[string][]resources.Resource) {
	var err error
	region := item.Region.Name
	_, ok := cache[region]
	if !ok {
		cache[region] = map[string][]resources.Resource{}
	}
	left, ok := cache[region][item.Type]
	if !ok {
		left, err = item.List()
		if err != nil {
			item.State = ItemStateFailed
			item.Reason = err.Error()
			return
		}
		cache[region][item.Type] = left
	}

	for _, r := range left {
		if item.Equals(r) {
			checker, ok := r.(resources.Filter)
			if ok {
				err := checker.Filter()
				if err != nil {
					break
				}
			}

			return
		}
	}

	item.State = ItemStateFinished
	item.Reason = ""
}
