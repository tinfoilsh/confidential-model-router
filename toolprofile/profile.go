package toolprofile

type Profile struct {
	Name            string
	ToolServerModel string
}

var WebSearch = Profile{
	Name:            "web_search",
	ToolServerModel: "websearch",
}
