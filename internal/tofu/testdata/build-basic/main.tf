resource "aws_instance" "foo" {
  ami = "bar"
  num = 2
}

resource "aws_instance" "bar" {
  foo = "bar"
}
