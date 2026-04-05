resource "aws_instance" "foo" {
  ami = "CHANGED"
  num = 99
}

resource "aws_instance" "bar" {
  foo = "bar"
}
