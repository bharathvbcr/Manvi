from slugify import slugify

def main():
    assert slugify("Hello World") == "hello-world"
    assert slugify("A  B") == "a-b"
    assert slugify("Rock & Roll!") == "rock-roll"
    print("ok")

main()
