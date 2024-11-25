## Step 1: Open a Pull Request

Open a Pull Request with description including pattern:

```regex
(?i)depend(?:s|ed|ing)?\s+on:\s+#(\d+)
```

- "Depends on #123"
- "dependent on #456"
- "depending on #789"
- "DEPEND ON #101"

## Step 2: Run `gh cascade`

![image](https://github.com/user-attachments/assets/362537f2-b26b-4e3b-9397-10c236f02b8b)
