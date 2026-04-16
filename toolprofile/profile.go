package toolprofile

type Profile struct {
	Name            string
	ToolServerModel string
	PromptName      string
}

var WebSearch = Profile{
	Name:            "web_search",
	ToolServerModel: "websearch",
	PromptName:      "openai_web_search",
}
