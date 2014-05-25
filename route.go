package main

// An ErrDeadlock represents a situation in which all of the instances have either
// finished or are waiting for a message.
type ErrDeadlock struct {
	// WaitingInstances lists the instances that are still alive and trying to receive a message.
	WaitingInstances  []int
	// RemainingMessages lists the pairs of instances that have unreceived messages between them.
	RemainingMessages []struct{ From, To int }
}

func (e ErrDeadlock) Error() string {
	return "wszystkie niezakończone instancje są zablokowane"
}

// An ErrRemainingMessages represents a situation when some messages were left
// in the queues (ie. weren't received) when all the instances have finished.
// This situation should not be considered an error, but we should warn about it.
type ErrRemainingMessages struct {
	// RemainingMessages lists the pairs of instances that have unreceived messages between them.
	RemainingMessages []struct{ From, To int }
}

func (e ErrRemainingMessages) Error() string {
	return "po zakończeniu działania pozostały nieodebrane wiadomości"
}

type requestAndId struct {
	id int
	r  *request
}

func merge(inputs []<-chan *request, fn func(*requestAndId) (int, bool)) (deadlocked []int) {
	blocked := make([]bool, len(inputs))
	lastInputs := make([]*request, len(inputs))
	for {
		for i, c := range inputs {
			if lastInputs[i] != nil || blocked[i] {
				continue
			}
			lastInputs[i] = <-c
		}
		firstI := -1
		for i, v := range lastInputs {
			if v == nil {
				continue
			}
			if firstI == -1 || v.time < lastInputs[firstI].time {
				firstI = i
			}
		}
		if firstI == -1 {
			// Either all the channels are closed or all the channels that aren't are in blocking requests.
			// In the latter case a deadlock has occurred, because nothing can unblock them anymore.
			var blockedInstances []int
			for i, b := range blocked {
				if b {
					blockedInstances = append(blockedInstances, i)
				}
			}
			return blockedInstances
		}
		i, block := fn(&requestAndId{id: firstI, r: lastInputs[firstI]})
		blocked[i] = block
		lastInputs[firstI] = nil
	}
}

type queueSet struct {
	queues    map[int][]*Message
	receiveFn func() (*response, bool)
	output    chan<- *response
}

func newQueueSet(output chan<- *response) *queueSet {
	return &queueSet{
		queues: make(map[int][]*Message),
		output: output,
	}
}

func (qs *queueSet) dequeue(from int) *Message {
	ms := qs.queues[from]
	if len(ms) > 1 {
		qs.queues[from] = ms[1:]
	} else {
		delete(qs.queues, from)
	}
	return ms[0]
}

// handleRequest handles a receive request from this instance of a send request
// to this instance. handleRequest returns true iff the instance is now blocked
// and won't emit any requests itself until unblocked by an incoming message.
func (qs *queueSet) handleRequest(req *requestAndId) (blocked bool) {
	switch req.r.requestType {
	case requestSend:
		qs.queues[req.id] = append(qs.queues[req.id],
			&Message{
				Source:   req.id,
				Target:   req.r.destination,
				SendTime: req.r.time,
				Message:  req.r.message,
			})
	case requestRecv:
		if qs.receiveFn != nil {
			panic("two simultaneous receives")
		}
		qs.receiveFn = func() (*response, bool) {
			if _, ok := qs.queues[req.r.source]; ok {
				return &response{message: qs.dequeue(req.r.source)}, true
			}
			return nil, false
		}
	case requestRecvAny:
		if qs.receiveFn != nil {
			panic("two simultaneous receives")
		}
		qs.receiveFn = func() (*response, bool) {
			for i := range qs.queues {
				return &response{message: qs.dequeue(i)}, true
			}
			return nil, false
		}
	}
	if qs.receiveFn != nil {
		if response, ok := qs.receiveFn(); ok {
			qs.output <- response
			qs.receiveFn = nil
		}
	}
	return qs.receiveFn != nil
}

// RouteMessages processes requests (send and receives) from a set of instances and sends back responses
// to requests that require them. It should be given two slices of equal size: requestChans[i] should
// be the channel that provides the requests from instance i and responses to that instance will be delivered
// to responseChans[i]. The function will return once all requests are processed and all input channels are closed,
// or once an error occurs. The function leaves output channels open.
//
// Prerequisites:
// Each output channel must be buffered.
// A request that requires a response must not be followed by another request until the response is read.
func RouteMessages(requestChans []<-chan *request, responseChans []chan<- *response) error {
	queueSets := make([]*queueSet, len(requestChans))
	for i, output := range responseChans {
		queueSets[i] = newQueueSet(output)
	}
	blocked := merge(requestChans, func(req *requestAndId) (int, bool) {
		var target int
		switch req.r.requestType {
		case requestSend:
			target = req.r.destination
		default:
			target = req.id
		}
		return target, queueSets[target].handleRequest(req)
	})
	var remaining []struct{ From, To int }
	for i, qs := range queueSets {
		for j := range qs.queues {
			remaining = append(remaining, struct{ From, To int }{j, i})
		}
	}
	if len(blocked) > 0 {
		return ErrDeadlock{WaitingInstances: blocked, RemainingMessages: remaining}
	}
	if len(remaining) > 0 {
		return ErrRemainingMessages{RemainingMessages: remaining}
	}
	return nil
}
