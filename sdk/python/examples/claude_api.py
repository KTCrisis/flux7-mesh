"""Example: governed tool use with the Claude API.

Requires: pip install agent-mesh[anthropic]
Requires: agent-mesh daemon running on localhost:9090
"""
from agent_mesh import GovernedToolkit

toolkit = GovernedToolkit(agent="my-agent")


@toolkit.tool
def get_weather(city: str) -> str:
    """Get current weather for a city."""
    return f"22C and sunny in {city}"


@toolkit.tool
def send_email(to: str, subject: str, body: str) -> str:
    """Send an email to a recipient."""
    print(f"  -> sending email to {to}")
    return f"email sent to {to}"


def main():
    import anthropic

    client = anthropic.Anthropic()
    messages = [{"role": "user", "content": "What's the weather in Paris? Then email the result to alice@example.com"}]

    while True:
        response = client.messages.create(
            model="claude-sonnet-4-6",
            max_tokens=1024,
            tools=toolkit.schemas(),
            messages=messages,
        )

        if response.stop_reason == "end_turn":
            for block in response.content:
                if hasattr(block, "text"):
                    print(block.text)
            break

        if response.stop_reason == "tool_use":
            tool_results = toolkit.process_response(
                [b.model_dump() for b in response.content]
            )
            messages.append({"role": "assistant", "content": [b.model_dump() for b in response.content]})
            messages.append({"role": "user", "content": tool_results})


if __name__ == "__main__":
    main()
