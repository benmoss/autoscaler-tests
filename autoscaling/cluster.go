package autoscaling

type Cluster interface {
	NodeCount() int
}
