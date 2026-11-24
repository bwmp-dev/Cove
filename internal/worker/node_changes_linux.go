package worker

func lockNodeChanges(root string) (func(), error) { return lockWorker(root) }
